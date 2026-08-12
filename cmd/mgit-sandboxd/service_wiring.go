package main

import (
	"context"
	"crypto/rand"
	"fmt"
	"log/slog"
	"path/filepath"
	"sync"
	"time"

	"github.com/oklog/ulid/v2"

	"github.com/hyper-swe/mgit/internal/model"
	"github.com/hyper-swe/mgit/internal/service"
	"github.com/hyper-swe/mgit/internal/store/index"
	"github.com/hyper-swe/mgit/internal/store/policy"
)

// sandboxIndexDB is the daemon's append-only sandbox audit trail
// (sandbox_events, FR-17.18), co-located with the host sandbox config
// (images.lock, trust root) under the host root — host-side data, never
// inside a worktree.
const sandboxIndexDB = "sandbox-index.db"

// buildSandboxService constructs the lifecycle service the daemon
// dispatches to: the supervised (ceiling-wrapped) manager, the append-only
// sandbox_events audit store, the host policy reader, the injected clock,
// and a host-owned ULID id generator. It returns the audit-event store
// (shared with the land path) and a closer for it. Handlers go through this
// service, never the manager directly (architecture rule).
//
// The policy store is INJECTED rather than opened here: the daemon resolves
// the FR-17.26 fleet ceiling from host policy before this point, and the
// ceiling and the launch path must read one policy, not two independently
// opened views of it. A nil store is refused — a serving daemon without a
// policy reader would enforce nothing (SEC-02).
// Refs: FR-17.13, FR-17.18, MGIT-11.10.8, MGIT-98
func buildSandboxService(manager model.SandboxManager, hostRoot string, policyStore *policy.Store,
	clock func() time.Time) (*service.SandboxService, *index.Store, func() error, error) {
	if policyStore == nil {
		return nil, nil, nil, fmt.Errorf("host policy store is required to serve sandbox operations")
	}
	// index.New creates the host root on the way to its own database, which
	// is what makes the first `mgit sandbox …` in a fresh repo work — nothing
	// else had created .mgit/sandbox by then. Refs: FR-17.13, MGIT-61.15
	events, err := index.New(filepath.Join(hostRoot, sandboxIndexDB), clock)
	if err != nil {
		return nil, nil, nil, fmt.Errorf("open sandbox audit index: %w", err)
	}
	svc, err := service.NewSandboxService(manager, events, policyStore, clock, newIDGen(clock))
	if err != nil {
		_ = events.Close()
		return nil, nil, nil, fmt.Errorf("wire sandbox service: %w", err)
	}
	return svc, events, events.Close, nil
}

// newIDGen returns a monotonic ULID generator for host-assigned sandbox
// IDs (sortable, cryptographically seeded). It is serialized so concurrent
// launches cannot race the monotonic entropy source. Refs: FR-17.9
func newIDGen(clock func() time.Time) func() (string, error) {
	entropy := ulid.Monotonic(rand.Reader, 0)
	var mu sync.Mutex
	return func() (string, error) {
		mu.Lock()
		defer mu.Unlock()
		id, err := ulid.New(ulid.Timestamp(clock().UTC()), entropy)
		if err != nil {
			return "", fmt.Errorf("generate sandbox id: %w", err)
		}
		return id.String(), nil
	}
}

// slogPolicyRecorder satisfies policy.EventRecorder by logging policy
// changes. The daemon only reads the effective policy (it never Saves), so
// this is a constructor requirement that records the rare change if one
// ever occurs through this process. Refs: FR-17.13
type slogPolicyRecorder struct{ logger *slog.Logger }

// RecordPolicyChange logs one effective-policy change.
func (r slogPolicyRecorder) RecordPolicyChange(_ context.Context, detail string) error {
	r.logger.Warn("sandbox policy changed", "event", "policy_change", "detail", detail)
	return nil
}
