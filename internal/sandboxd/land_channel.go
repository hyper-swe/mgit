package sandboxd

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"log/slog"
	"net"
	"sync"

	"github.com/hyper-swe/mgit/internal/sandboxd/land"
)

// LandDialer opens a raw, host-initiated connection to a guest's land
// channel (the host dials the guest land port, mirroring exec). The
// firecracker backend's NewLandDialer satisfies it; injected so the land
// channel is testable without a VM. Refs: FR-17.5
type LandDialer interface {
	DialGuest(ctx context.Context, sandboxID string) (net.Conn, error)
}

// LandChannel is the host side of the guest land pull. It resolves the
// sandbox's launch-bound peer and authorizes it (SEC-10, fail closed on an
// unbound or torn-down sandbox), dials the guest land channel, and reads the
// whole framed object pool ONCE into a bounded in-memory buffer (the host
// land budget, FR-17.35). It then replays that exact buffer to the verified
// orchestrator via OpenLandStream — so the bytes the orchestrator verifies
// and imports are the very bytes the host already read, closing the SEC-06
// verify-then-refetch window. One pull serves one land; the buffer is
// consumed on replay. Refs: FR-17.5, FR-17.35, SEC-06, SEC-10
type LandChannel struct {
	binder *PeerBinder
	dialer LandDialer
	limits land.Limits
	logger *slog.Logger

	mu    sync.Mutex
	pools map[string][]byte // sandbox ID -> pulled raw pool, awaiting replay
}

// NewLandChannel wires the land channel. limits bound one pulled pool (a
// hostile guest must never drive an unbounded host allocation); pass
// land.DefaultLimits().
func NewLandChannel(binder *PeerBinder, dialer LandDialer, limits land.Limits, logger *slog.Logger) *LandChannel {
	return &LandChannel{
		binder: binder, dialer: dialer, limits: limits, logger: logger,
		pools: make(map[string][]byte),
	}
}

// Pull authorizes the sandbox's bound peer (SEC-10), dials the guest land
// channel, reads the whole framed pool once into a bounded buffer kept for
// the orchestrator to replay, and returns the decoded pool for host-side
// batch derivation. The single network read is what the orchestrator later
// verifies and imports (SEC-06: no verify-then-refetch). An unbound/torn-down
// sandbox is refused before any dial; an over-budget or malformed pool is
// refused and nothing is buffered. Refs: FR-17.5, FR-17.35, SEC-06, SEC-10
func (c *LandChannel) Pull(ctx context.Context, sandboxID string) ([]land.Object, error) {
	// Resolve the launch-bound peer and authorize it BEFORE any connection,
	// so the host only ever pulls from the exact peer bound at launch and
	// refuses an unbound or torn-down sandbox (SEC-10, fail closed).
	boundPeer, _ := c.binder.BoundPeer(sandboxID)
	if err := c.binder.Authorize(sandboxID, boundPeer); err != nil {
		return nil, err
	}
	conn, err := c.dialer.DialGuest(ctx, sandboxID)
	if err != nil {
		return nil, fmt.Errorf("sandbox land: dial guest land channel: %w", err)
	}
	defer func() { _ = conn.Close() }()
	if dl, ok := ctx.Deadline(); ok {
		_ = conn.SetDeadline(dl)
	}

	// Read one byte past the ceiling so an over-budget pool is detected
	// rather than silently truncated.
	raw, err := io.ReadAll(io.LimitReader(conn, c.limits.MaxTotalBytes+1))
	if err != nil {
		return nil, fmt.Errorf("sandbox land: read pool: %w", err)
	}
	if int64(len(raw)) > c.limits.MaxTotalBytes {
		return nil, fmt.Errorf("sandbox land: pool exceeds the %d-byte host budget", c.limits.MaxTotalBytes)
	}
	// Decode under the full ceilings (per-object/count/total) for host-side
	// derivation; the same bytes are replayed and re-decoded by the orchestrator.
	objs, err := c.limits.DecodeObjects(bytes.NewReader(raw))
	if err != nil {
		return nil, fmt.Errorf("sandbox land: %w", err)
	}

	c.mu.Lock()
	c.pools[sandboxID] = raw
	c.mu.Unlock()
	return objs, nil
}

// OpenLandStream replays the buffer pulled for this sandbox, satisfying the
// orchestrator's service.LandStreamOpener port. The buffer is consumed: a
// land pull is one-shot, and a second open without a fresh Pull is an error.
func (c *LandChannel) OpenLandStream(_ context.Context, sandboxID string) (io.ReadCloser, error) {
	c.mu.Lock()
	raw, ok := c.pools[sandboxID]
	delete(c.pools, sandboxID)
	c.mu.Unlock()
	if !ok {
		return nil, fmt.Errorf("sandbox land: no pulled pool for sandbox %s", sandboxID)
	}
	return io.NopCloser(bytes.NewReader(raw)), nil
}

// Discard drops any buffer pulled for a sandbox without replaying it, so an
// aborted land (e.g. derivation rejected the batch before the orchestrator
// ran) leaves no buffer pinned in memory.
func (c *LandChannel) Discard(sandboxID string) {
	c.mu.Lock()
	delete(c.pools, sandboxID)
	c.mu.Unlock()
}
