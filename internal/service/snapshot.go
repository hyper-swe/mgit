package service

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/hyper-swe/mgit/internal/model"
)

// WorktreeCapturer is the store-side capability SnapshotService drives. It is
// an interface so the quiescence logic — the part with the interesting
// behavior — is testable without a repository on disk. Refs: MGIT-110
type WorktreeCapturer interface {
	// Fingerprint reports the working tree's content fingerprint.
	Fingerprint() (string, error)
	// Capture records the current working tree as a passive snapshot.
	Capture(ctx context.Context, taskID string, at time.Time) (*model.Snapshot, error)
	// Prune enforces the snapshot namespace's own retention.
	Prune(ctx context.Context, taskID string, keep int) (int, error)
}

// taskWatch is the per-task quiescence state.
type taskWatch struct {
	lastSeen     string // fingerprint at the previous observation
	lastCaptured string // fingerprint of the most recent successful capture
	started      bool   // an observation has established a baseline
}

// SnapshotService decides WHEN a passive snapshot is worth taking.
//
// The rule is quiescence: capture a state that has stopped changing, once. Not
// every tick — that records torn, half-written trees and buries the useful
// ones among near-duplicates. Not on a fixed schedule either, which snapshots
// the middle of an edit as readily as the end of one.
//
// Nothing here asks the agent for anything, which is the whole design. R-H234:
// recovery must not depend on virtue, because the case that failed (MGIT-109)
// was an agent that made zero commits in thirty minutes and lost all of it.
// Refs: MGIT-110, MGIT-109, R-H234
type SnapshotService struct {
	mu      sync.Mutex
	store   WorktreeCapturer
	clock   func() time.Time
	keep    int
	watches map[string]*taskWatch
}

// DefaultSnapshotRetention is how many passive snapshots a task keeps.
//
// Snapshots are evidence with a working life, not content with a permanent
// one: they exist so an interrupted run can be recovered and so a reviewer can
// see when work actually changed. Neither purpose needs the whole history, and
// an unbounded namespace would make `mgit gc` the only thing standing between
// a long run and an ever-growing store. Refs: MGIT-110
const DefaultSnapshotRetention = 20

// NewSnapshotService builds a SnapshotService. keep <= 0 selects
// DefaultSnapshotRetention. Refs: MGIT-110
func NewSnapshotService(store WorktreeCapturer, clock func() time.Time, keep int) *SnapshotService {
	if keep <= 0 {
		keep = DefaultSnapshotRetention
	}
	return &SnapshotService{store: store, clock: clock, keep: keep, watches: map[string]*taskWatch{}}
}

// Observe is one tick of the passive watcher. It returns the Snapshot when
// this tick captured one, and nil when there was nothing to capture.
//
// The state machine, per task:
//
//	fingerprint == lastCaptured -> nothing changed since the last capture.
//	fingerprint != lastSeen     -> still being edited; note it and wait.
//	fingerprint == lastSeen     -> SETTLED and not yet captured: capture.
//
// A task's FIRST observation only establishes a baseline. The one exception
// worth noting is that a worktree already settled when the watcher starts is
// captured on the second tick — an agent that did its editing before the
// sandbox came up is still recoverable. Refs: MGIT-110
func (s *SnapshotService) Observe(ctx context.Context, taskID string) (*model.Snapshot, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	fingerprint, err := s.store.Fingerprint()
	if err != nil {
		return nil, fmt.Errorf("snapshot watch %s: %w", taskID, err)
	}
	w, ok := s.watches[taskID]
	if !ok {
		w = &taskWatch{}
		s.watches[taskID] = w
	}
	switch {
	case !w.started:
		w.started, w.lastSeen = true, fingerprint
		return nil, nil
	case fingerprint == w.lastCaptured:
		w.lastSeen = fingerprint
		return nil, nil
	case fingerprint != w.lastSeen:
		w.lastSeen = fingerprint
		return nil, nil
	}
	return s.captureLocked(ctx, taskID, w, fingerprint)
}

// captureLocked records a settled worktree and applies retention. The caller
// holds the lock.
//
// lastCaptured is advanced ONLY on success. A failed capture that marked the
// state as captured would never be retried, and one transient error would
// silently cost a whole run its recovery point — the failure mode this feature
// exists to remove, reintroduced by its own bookkeeping. Refs: MGIT-110
func (s *SnapshotService) captureLocked(ctx context.Context, taskID string, w *taskWatch, fingerprint string) (*model.Snapshot, error) {
	snap, err := s.store.Capture(ctx, taskID, s.clock())
	if err != nil {
		return nil, fmt.Errorf("snapshot watch %s: capture: %w", taskID, err)
	}
	w.lastCaptured = fingerprint
	if _, err := s.store.Prune(ctx, taskID, s.keep); err != nil {
		// The snapshot IS recorded; retention is housekeeping. Reporting the
		// prune failure as a capture failure would make the caller retry a
		// capture that already succeeded.
		return snap, fmt.Errorf("snapshot watch %s: captured, but retention failed: %w", taskID, err)
	}
	return snap, nil
}

// Rebind points the service at a freshly opened store while KEEPING the
// per-task quiescence state.
//
// The production watcher opens the worktree per pass and closes it again, so
// the store handle is short-lived by design; the fingerprint history is not.
// Losing it between passes would make every pass look like a first
// observation, nothing would ever settle, and no snapshot would ever be taken
// — a watcher that runs forever and captures nothing. Refs: MGIT-110
func (s *SnapshotService) Rebind(store WorktreeCapturer) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.store = store
}
