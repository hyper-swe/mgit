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
//
// What the memory dimension counts, stated plainly because the distinction
// matters to anyone reading a refusal: ADMITTED memory — the sum of what each
// live sandbox DECLARED — not resident memory. libkrun and firecracker
// allocate guest pages lazily, so a 4096 MB guest touching 300 MB still holds
// its whole declared share here. That is the conservative direction (mgit
// under-admits rather than over-commits the host), but this ceiling is an
// admission budget and not a measurement of host memory pressure, and it
// should never be described as one. Refs: FR-17.26, MGIT-98
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

	if err := c.admit(count, usedMB, requestMB); err != nil {
		return err
	}

	c.reservedCount++
	c.reservedMB += requestMB
	return nil
}

// admit is the ceiling's whole decision, expressed as three refusals that must
// stay tellable apart.
//
// Every one of them says something about the HOST, never about the request
// being individually oversized — that is model.ErrSandboxResourceLimitExceeded,
// raised in the service before this decorator is reached, and it has the
// opposite fix ("ask for less"). None of these may borrow its language.
//
//  1. too many sandboxes  -> free one or wait
//  2. bigger than the WHOLE ceiling -> nothing to wait for; the host is too
//     small for the memory policy in force
//  3. bigger than what is left -> free capacity or wait
//
// The second exists because of the small-host case: 50% of a modest host's
// memory can land below the 2048 MB per-sandbox default, so every launch —
// including one that declared nothing at all — is unfittable. Telling that
// operator to "free capacity or wait" is advice that can never work.
// Refs: FR-17.26, R-H212, MGIT-98
func (c *CeilingManager) admit(count, usedMB, requestMB int) error {
	if c.maxConcurrent > 0 && count >= c.maxConcurrent {
		return fmt.Errorf("%w: the host is already running or admitting %d sandboxes (host-wide cap %d); "+
			"this launch is not too big — free capacity with `mgit sandbox remove <task>` or wait for one to expire",
			model.ErrSandboxCeilingExceeded, count, c.maxConcurrent)
	}
	if c.maxTotalMemoryMB <= 0 {
		return nil
	}
	if requestMB > c.maxTotalMemoryMB {
		return fmt.Errorf("%w: this launch needs %d MB but the host-wide sandbox memory ceiling is %d MB "+
			"in total, so it cannot be admitted even on a completely idle host; this host is too small for "+
			"the memory policy in force — raise `max_total_memory_percent` in host sandbox policy, start "+
			"mgit-sandboxd with an explicit --max-memory-mb, or use a larger host",
			model.ErrSandboxCeilingExceeded, requestMB, c.maxTotalMemoryMB)
	}
	if usedMB+requestMB > c.maxTotalMemoryMB {
		return fmt.Errorf("%w: %d MB in use or admitted + %d MB requested exceeds the %d MB host-wide ceiling; "+
			"free capacity with `mgit sandbox remove <task>` or wait for one to expire",
			model.ErrSandboxCeilingExceeded, usedMB, requestMB, c.maxTotalMemoryMB)
	}
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

// SyncWorktree forwards the OPTIONAL worktree-sync capability to the inner
// backend, and reports it unsupported when the inner backend does not have it.
//
// It is here because this decorator wraps EVERY backend: an optional
// capability a decorator does not forward simply disappears, and the service's
// type assertion would then see an unsyncable sandbox no matter which backend
// is running. The forward is unconditional in both directions — never
// inventing the capability either, so firecracker's real limitation stays a
// refusal rather than becoming a silent success.
//
// The ceiling itself has nothing to admit here: a sync consumes no VM slot and
// no additional memory. Refs: MGIT-76, FR-17.26, ADR-011
func (c *CeilingManager) SyncWorktree(ctx context.Context, id string, opts model.WorktreeSyncOptions) (*model.WorktreeSyncReport, error) {
	syncer, ok := c.inner.(model.WorktreeSyncer)
	if !ok {
		return nil, fmt.Errorf("%w: backend does not deliver the worktree as a live shared directory",
			model.ErrSandboxSyncUnsupported)
	}
	return syncer.SyncWorktree(ctx, id, opts)
}

// ExportArtifact forwards the OPTIONAL guest->host artifact export capability
// to the inner backend when it has one, and reports the limitation when it
// does not.
//
// The ceiling wraps EVERY manager, so without this passthrough the capability
// would be invisible to the service no matter which backend is running. It is
// a pure delegation: an export consumes no VM resources, so there is nothing
// for the ceiling itself to admit. Refs: MGIT-73, FR-17.26, ADR-011
func (c *CeilingManager) ExportArtifact(ctx context.Context, id string,
	req model.ArtifactExportRequest) (*model.ArtifactExportResult, error) {
	exporter, ok := c.inner.(model.ArtifactExporter)
	if !ok {
		return nil, fmt.Errorf("%w: this backend has no artifact export path", model.ErrArtifactExportUnsupported)
	}
	return exporter.ExportArtifact(ctx, id, req)
}
