package sandboxd

import (
	"context"
	"fmt"
	"sync"

	"github.com/hyper-swe/mgit/internal/model"
)

// fallbackAccountedMemoryMB backs the injectable default when the
// caller does not supply one (NFR-17.5). An unresolved or negative
// memory request is never accounted as free — that would let an agent
// loop bypass the ceiling by omitting the field (SEC-09).
const fallbackAccountedMemoryMB = 2048

// CeilingManager decorates a SandboxManager with the host-wide
// resource ceiling (FR-17.26, SEC-09): per-VM caps alone cannot stop
// an agent loop from exhausting the host. Launches beyond either cap
// fail fast with ErrSandboxCeilingExceeded. A zero cap disables that
// dimension; the caller derives caps and the accounted default from
// the host policy store (model.SandboxPolicy). Admission uses a
// reservation: capacity is reserved under the lock, the (possibly
// slow) backend launch runs OUTSIDE it, and the reservation is dropped
// once the sandbox is visible via List — racing launches can never
// overshoot, and one cold boot never serializes other operations.
// Backends MUST register a sandbox in List before Launch returns.
// Refs: FR-17.26
type CeilingManager struct {
	inner            model.SandboxManager
	maxConcurrent    int
	maxTotalMemoryMB int
	defaultMemoryMB  int

	mu            sync.Mutex
	reservedCount int
	reservedMB    int
}

// NewCeilingManager wraps inner with the global ceiling.
// defaultMemoryMB is the memory accounted for requests that leave
// MemoryMB unresolved; values <= 0 select the NFR-17.5 fallback.
func NewCeilingManager(inner model.SandboxManager, maxConcurrent, maxTotalMemoryMB, defaultMemoryMB int) *CeilingManager {
	if defaultMemoryMB <= 0 {
		defaultMemoryMB = fallbackAccountedMemoryMB
	}
	return &CeilingManager{
		inner:            inner,
		maxConcurrent:    maxConcurrent,
		maxTotalMemoryMB: maxTotalMemoryMB,
		defaultMemoryMB:  defaultMemoryMB,
	}
}

// Launch admits the request against the ceiling, then delegates.
// Refs: FR-17.26
func (c *CeilingManager) Launch(ctx context.Context, opts model.SandboxLaunchOptions) (*model.SandboxInfo, error) {
	requestMB := opts.MemoryMB
	if requestMB <= 0 {
		requestMB = c.defaultMemoryMB
	}

	if err := c.reserve(ctx, requestMB); err != nil {
		return nil, err
	}
	info, err := c.inner.Launch(ctx, opts)
	c.unreserve(requestMB)
	if err != nil {
		return nil, err
	}
	return info, nil
}

// reserve admits one launch and holds its capacity until the backend
// registers the sandbox. Existing usage is recomputed from the inner
// List (restart-safe — no shadow state to lose).
func (c *CeilingManager) reserve(ctx context.Context, requestMB int) error {
	c.mu.Lock()
	defer c.mu.Unlock()

	count, usedMB, err := c.usage(ctx)
	if err != nil {
		return fmt.Errorf("ceiling admission: %w", err)
	}
	count += c.reservedCount
	usedMB += c.reservedMB

	if c.maxConcurrent > 0 && count >= c.maxConcurrent {
		return fmt.Errorf("%w: %d sandboxes running or admitted (cap %d)",
			model.ErrSandboxCeilingExceeded, count, c.maxConcurrent)
	}
	if c.maxTotalMemoryMB > 0 && usedMB+requestMB > c.maxTotalMemoryMB {
		return fmt.Errorf("%w: %d MB in use or admitted + %d MB requested exceeds %d MB ceiling",
			model.ErrSandboxCeilingExceeded, usedMB, requestMB, c.maxTotalMemoryMB)
	}

	c.reservedCount++
	c.reservedMB += requestMB
	return nil
}

// unreserve releases one admission's reservation.
func (c *CeilingManager) unreserve(requestMB int) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.reservedCount--
	c.reservedMB -= requestMB
}

// usage sums live sandboxes and their attributable memory from the
// inner manager — the restart-safe source of truth.
func (c *CeilingManager) usage(ctx context.Context) (count, usedMB int, err error) {
	sandboxes, err := c.inner.List(ctx)
	if err != nil {
		return 0, 0, err
	}
	for _, sb := range sandboxes {
		if sb.State == model.StateDestroyed {
			continue
		}
		count++
		if sb.MemoryMB > 0 {
			usedMB += sb.MemoryMB
		} else {
			usedMB += c.defaultMemoryMB
		}
	}
	return count, usedMB, nil
}

// List delegates to the inner manager.
func (c *CeilingManager) List(ctx context.Context) ([]model.SandboxInfo, error) {
	return c.inner.List(ctx)
}

// Exec delegates to the inner manager.
func (c *CeilingManager) Exec(ctx context.Context, id string, req model.ExecRequest) (*model.ExecResult, error) {
	return c.inner.Exec(ctx, id, req)
}

// Stop delegates to the inner manager.
func (c *CeilingManager) Stop(ctx context.Context, id string, force bool) error {
	return c.inner.Stop(ctx, id, force)
}

// Remove delegates to the inner manager; freed capacity is visible to
// the next admission via List.
func (c *CeilingManager) Remove(ctx context.Context, id string, force bool) error {
	return c.inner.Remove(ctx, id, force)
}

// Resolve delegates to the inner manager.
func (c *CeilingManager) Resolve(ctx context.Context, id string) (*model.SandboxInfo, error) {
	return c.inner.Resolve(ctx, id)
}
