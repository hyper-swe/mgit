package service

import (
	"context"
	"fmt"

	"github.com/hyper-swe/mgit/internal/model"
)

// RehydrateReport is what one daemon-start reconciliation did, by task ID:
// which registrations came back live, and which were discarded because their
// VM could not be verified. Both lists are reported (and logged by the daemon)
// because "nothing was recovered" and "everything was discarded" are very
// different facts about a host. Refs: MGIT-102
type RehydrateReport struct {
	Recovered []string `json:"recovered"`
	Discarded []string `json:"discarded"`
}

// lostVMDetail is the audit detail recorded when a rehydrated registration
// claimed a VM this daemon cannot verify. It states what was actually
// established — that the sandbox is no longer supervised — rather than
// claiming the VM was observed to be gone, which this daemon cannot know.
const lostVMDetail = `{"reason":"unsupervised: the daemon that supervised this sandbox exited; its VM could not be verified by the daemon that replaced it"}`

// Rehydrate rebuilds the in-process working set from the durable registry at
// daemon start, RECONCILING each row against reality rather than trusting it.
//
// The rule is that no state is reported unless it has been verified:
//
//   - `created` (never booted) asserts no VM at all — the normal state of a
//     `mgit work --sandbox` registration under lazy provisioning (FR-17.9,
//     FR-17.10). There is nothing to verify, and it comes back as `created`.
//   - `running` / `suspended` assert that a VM exists. They are verified
//     against the backend. A sandbox the backend can still resolve comes back
//     with the state the BACKEND reports; one it cannot is discarded, with a
//     terminal `killed` event recorded so the trail never ends at `created`
//     for a sandbox that is gone.
//
// Reporting `running` for a VM this daemon cannot see would be a worse defect
// than the amnesia this fixes, because it would be confidently wrong.
//
// It is idempotent: a task already in the working set is left alone, so a
// second call recovers nothing and audits nothing.
// Refs: FR-17.9, FR-17.10, FR-17.18, MGIT-102
func (s *SandboxService) Rehydrate(ctx context.Context) (RehydrateReport, error) {
	report := RehydrateReport{}
	if s.registry == nil {
		return report, nil
	}
	rows, err := s.registry.ListSandboxes(ctx)
	if err != nil {
		// A daemon that cannot read the roster must say so, never start
		// serving as though the roster were empty — starting empty IS the
		// reported defect.
		return report, fmt.Errorf("sandbox rehydrate: read registry: %w", err)
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	for i := range rows {
		row := rows[i]
		if _, live := s.byTask[row.Info.TaskID]; live {
			continue // already adopted; re-adopting would double-count and re-audit
		}
		state, booted, ok := s.verifyLocked(ctx, row)
		if !ok {
			if err := s.discardLostLocked(ctx, row); err != nil {
				return report, err
			}
			report.Discarded = append(report.Discarded, row.Info.TaskID)
			continue
		}
		s.adoptLocked(row, state, booted)
		report.Recovered = append(report.Recovered, row.Info.TaskID)
	}
	return report, nil
}

// verifyLocked establishes what a persisted row's state ACTUALLY is now.
// A `created` row is self-consistent (it claims no VM). Any other state claims
// a VM, so the backend is asked; ok is false when the claim cannot be
// substantiated. Caller holds the lock. Refs: MGIT-102
func (s *SandboxService) verifyLocked(ctx context.Context, row model.SandboxRegistration) (state string, booted, ok bool) {
	if row.Info.State == model.StateCreated {
		return model.StateCreated, false, true
	}
	live, err := s.manager.Resolve(ctx, row.Info.ID)
	if err != nil || live == nil {
		return "", false, false
	}
	switch live.State {
	case model.StateRunning:
		return model.StateRunning, true, true
	case model.StateSuspended:
		return model.StateSuspended, false, true
	case model.StateCreated:
		return model.StateCreated, false, true
	default:
		// The backend knows this sandbox as landed or destroyed: it is over,
		// whatever the registry row said.
		return "", false, false
	}
}

// adoptLocked puts a verified registration back into the working set with the
// state that was established, not the state that was claimed. The idle-suspend
// clock restarts at adoption (a per-process timer, deliberately not persisted);
// the TTL deadline does NOT — it is absolute host-clock time, so a restart must
// not silently extend a sandbox's life. Caller holds the lock.
// Refs: NFR-17.3, FR-17.9, MGIT-102
func (s *SandboxService) adoptLocked(row model.SandboxRegistration, state string, booted bool) {
	info := row.Info
	info.State = state
	s.byTask[info.TaskID] = &sandboxReg{
		info:         info,
		opts:         row.LaunchOptions(),
		booted:       booted,
		lastActivity: s.clock().UTC(),
		expiresAt:    info.ExpiresAt,
	}
}

// discardLostLocked ends a registration whose VM could not be verified: it
// appends the terminal `killed` event (so the append-only trail never ends at
// `created` for a sandbox that no longer exists — the second, independent
// defect of MGIT-102) and then drops the durable row.
//
// The audit is written BEFORE the row is dropped: a row dropped first could
// leave a sandbox with no terminal event at all if the audit then failed, which
// is precisely the misleading trail being fixed. Caller holds the lock.
// Refs: FR-17.18, MGIT-102
func (s *SandboxService) discardLostLocked(ctx context.Context, row model.SandboxRegistration) error {
	if err := s.events.AppendSandboxEvent(ctx, &model.SandboxEvent{
		SandboxID: row.Info.ID, TaskID: row.Info.TaskID, EventType: model.EventKilled,
		ImageDigest: row.Info.ImageDigest, NetworkMode: row.Info.NetworkMode,
		Detail: lostVMDetail,
	}); err != nil {
		return fmt.Errorf("sandbox rehydrate: audit lost sandbox %s: %w", row.Info.ID, err)
	}
	if err := s.dropPersisted(ctx, row.Info.ID); err != nil {
		return fmt.Errorf("sandbox rehydrate: %w", err)
	}
	return nil
}
