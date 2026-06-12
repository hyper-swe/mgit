package sandboxd

import (
	"context"
	"fmt"
	"sync"

	"github.com/hyper-swe/mgit/internal/model"
)

// defaultAccountedMemoryMB is the memory charged to a launch whose
// request leaves MemoryMB unresolved (0 = policy default, NFR-17.5).
// An unresolved request is never accounted as free — that would let an
// agent loop bypass the ceiling by omitting the field (SEC-09).
const defaultAccountedMemoryMB = 2048

// CeilingManager decorates a SandboxManager with the host-wide
// resource ceiling (FR-17.26, SEC-09): per-VM caps alone cannot stop
// an agent loop from exhausting the host. Launches beyond either cap
// fail fast with ErrSandboxCeilingExceeded. A zero cap disables that
// dimension; the caller derives caps from the host policy store
// (model.SandboxPolicy.MaxConcurrentSandboxes / MaxTotalMemoryPercent
// resolved against host RAM). Refs: FR-17.26
type CeilingManager struct {
	inner            model.SandboxManager
	maxConcurrent    int
	maxTotalMemoryMB int

	// mu serializes admission: existing usage is recomputed from the
	// inner List (restart-safe — no shadow state to lose), and holding
	// the lock across the inner Launch prevents racing launches from
	// overshooting the cap.
	mu sync.Mutex
}

// NewCeilingManager wraps inner with the global ceiling.
func NewCeilingManager(inner model.SandboxManager, maxConcurrent, maxTotalMemoryMB int) *CeilingManager {
	return &CeilingManager{
		inner:            inner,
		maxConcurrent:    maxConcurrent,
		maxTotalMemoryMB: maxTotalMemoryMB,
	}
}

// Launch admits the request against the ceiling, then delegates.
// Refs: FR-17.26
func (c *CeilingManager) Launch(ctx context.Context, opts model.SandboxLaunchOptions) (*model.SandboxInfo, error) {
	c.mu.Lock()
	defer c.mu.Unlock()

	count, usedMB, err := c.usage(ctx)
	if err != nil {
		return nil, fmt.Errorf("ceiling admission: %w", err)
	}

	if c.maxConcurrent > 0 && count >= c.maxConcurrent {
		return nil, fmt.Errorf("%w: %d sandboxes running (cap %d)",
			model.ErrSandboxCeilingExceeded, count, c.maxConcurrent)
	}

	requestMB := opts.MemoryMB
	if requestMB == 0 {
		requestMB = defaultAccountedMemoryMB
	}
	if c.maxTotalMemoryMB > 0 && usedMB+requestMB > c.maxTotalMemoryMB {
		return nil, fmt.Errorf("%w: %d MB in use + %d MB requested exceeds %d MB ceiling",
			model.ErrSandboxCeilingExceeded, usedMB, requestMB, c.maxTotalMemoryMB)
	}

	return c.inner.Launch(ctx, opts)
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
			usedMB += defaultAccountedMemoryMB
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
