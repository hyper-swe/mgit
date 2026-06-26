// Package service: SyncService implements ADR-008 auto-housekeeping — keeping
// the `.mgit` base coherent with the project's current LOCAL working state with
// no manual `mgit sync` chore. See sync.go for the design.
package service

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/hyper-swe/mgit/internal/model"
	gitstore "github.com/hyper-swe/mgit/internal/store/git"
	"github.com/hyper-swe/mgit/internal/store/gitref"
	"github.com/hyper-swe/mgit/internal/store/index"
)

// assertPinnedForkBase enforces ADR-008 §4: when a task has a pinned fork-base
// (recorded at `mgit work`/`worktree add` time), the base that squash/diff
// computes against — the first micro-commit's parent — MUST equal it. Under
// mgit's append-only model they are equal by construction (a base resync
// advances only the SHARED base branch, never the task branch), so a mismatch
// means the task branch was retargeted or rewritten; fail loud rather than
// export a wrong net diff. Tasks with no worktree (committed directly, hence no
// pin) and root commits (empty computed base) skip the check. This is what makes
// the stored fork-base load-bearing rather than advisory. Refs: MGIT-35, ADR-008 §4
func assertPinnedForkBase(ctx context.Context, idx *index.Store, taskID, computedBase string) error {
	wt, err := idx.GetWorktreeByTask(ctx, taskID)
	if errors.Is(err, model.ErrWorktreeNotFound) {
		return nil // task committed directly (no worktree) → no pin to enforce
	}
	if err != nil {
		return fmt.Errorf("assert pinned fork-base: %w", err)
	}
	if wt.ForkBase == "" || computedBase == "" {
		return nil // nothing pinned, or a root commit with no parent
	}
	if wt.ForkBase != computedBase {
		return fmt.Errorf("%w: task %s pinned fork-base %s != computed base %s (task branch retargeted?)",
			model.ErrVerificationFailed, taskID, short(wt.ForkBase), short(computedBase))
	}
	return nil
}

// localStateReader reads the project's git truth READ-ONLY (the current local
// HEAD commit id). It is an interface so the resync logic is unit-testable
// without a real `.git` and so the defensive gitref reader is injected, not
// hard-wired. Refs: MGIT-35, ADR-008 §5
type localStateReader func(projectRoot string) (*gitref.LocalState, error)

// SyncService self-heals the `.mgit` base so a NEW worktree always carries the
// project's current local working state, eliminating the manual
// `mgit add . && mgit commit` resync. It runs a CHEAP content-based drift gate
// before every base-dependent command and resyncs only on real drift,
// transactionally, under the caller's already-held store lock. Pinned per-task
// fork-bases are never touched — only the shared base branch advances.
// Refs: MGIT-35, ADR-008 §3,§4,§6
type SyncService struct {
	repo        *gitstore.Repository
	worktree    *gitstore.WorktreeStore
	commitStore *gitstore.CommitStore
	readLocal   localStateReader
	clock       func() time.Time
	// boundTask is non-empty when the App is a linked worktree; a worktree has a
	// pinned fork-base and must NEVER resync (ADR-008 §3). Refs: MGIT-35
	boundTask string
}

// NewSyncService creates a SyncService. boundTask is the App's BoundTask (empty
// for a normal store, non-empty inside a linked worktree). Refs: MGIT-35
func NewSyncService(repo *gitstore.Repository, ws *gitstore.WorktreeStore, cs *gitstore.CommitStore,
	boundTask string, clock func() time.Time) *SyncService {
	return &SyncService{
		repo:        repo,
		worktree:    ws,
		commitStore: cs,
		readLocal:   gitref.ReadLocalState,
		clock:       clock,
		boundTask:   boundTask,
	}
}

// withLocalReader overrides the git-state reader (test seam). Refs: MGIT-35
func (s *SyncService) withLocalReader(r localStateReader) *SyncService {
	s.readLocal = r
	return s
}

// EnsureSynced is the auto-housekeeping gate. It runs the cheap content drift
// check and, only on real drift, resyncs the `.mgit` base from the current
// local working state. It is a no-op inside a linked worktree (pinned fork-base,
// ADR-008 §3) and degrades — not hard-fails — when the project has no readable
// git (gitref.ErrNoGit): the `.mgit` base is then simply used as-is. Any other
// git-read failure FAILS LOUD so mgit never materializes/diffs against a
// known-stale base. Refs: MGIT-35, ADR-008 §3,§6
func (s *SyncService) EnsureSynced(ctx context.Context) error {
	if s.boundTask != "" {
		return nil // worktree: pinned fork-base, never resync.
	}
	local, err := s.readLocal(s.repo.Root())
	if err != nil {
		// No git, or git present but HEAD has no commit yet (unborn branch /
		// detached-without-commit): there is nothing to sync FROM, so degrade
		// to the .mgit base rather than block the command. Only states that
		// could SILENTLY corrupt the base (shallow/sparse/unreadable) fail loud.
		if errors.Is(err, gitref.ErrNoGit) || errors.Is(err, gitref.ErrDetachedOrUnborn) {
			return nil
		}
		return fmt.Errorf("sync: read git state: %w", err)
	}
	liveWT, err := s.repo.WorkingTreeFingerprint()
	if err != nil {
		return fmt.Errorf("sync: working-tree fingerprint: %w", err)
	}
	stored, found, err := s.repo.ReadSyncState()
	if err != nil {
		return fmt.Errorf("sync: read state: %w", err)
	}
	if found && stored.GitHead == local.HeadCommit && stored.WorkTreeHash == liveWT {
		return nil // cheap path: no drift, no reimport.
	}
	return s.resync(ctx, local, liveWT)
}

// resync brings the base up to the current local working state TRANSACTIONALLY:
// it stages every trackable working file and, if that changes the base tree,
// APPENDS a base commit advancing the base branch (never rewrites — append-only,
// ADR-008 §6); if nothing changed it appends nothing. Only after the base is
// durable does it record the new drift signal, so an interrupted resync simply
// re-runs next time and converges (the content-addressed store makes a repeated
// import a no-op).
//
// It is STAGING-NEUTRAL: the gate fires on read-ish commands (`mgit status`,
// `mgit diff`), so a user's manual partial staging selection is snapshotted up
// front and restored afterward — resync absorbs the working tree into the base
// without destroying what the user staged for their next commit (ADR-008 §3).
// Refs: MGIT-35, ADR-008 §3,§6
func (s *SyncService) resync(ctx context.Context, local *gitref.LocalState, liveWT string) error {
	stagedBefore, err := s.repo.StagedSnapshot()
	if err != nil {
		return fmt.Errorf("sync: snapshot staging: %w", err)
	}
	if err := s.worktree.Add(ctx, "."); err != nil {
		return fmt.Errorf("sync: stage working tree: %w", err)
	}
	clean, err := s.commitStore.StagedTreeMatchesHead()
	if err != nil {
		return fmt.Errorf("sync: %w", err)
	}
	baseCommit, err := s.applyResync(ctx, clean, local)
	if err != nil {
		return err
	}
	// Restore the user's staging selection, undoing the Add(".") side effect so
	// the read-ish command that triggered the resync leaves staging untouched.
	if err := s.repo.RestoreStaging(stagedBefore); err != nil {
		return fmt.Errorf("sync: restore staging: %w", err)
	}
	return s.repo.WriteSyncState(gitstore.SyncState{
		GitHead:      local.HeadCommit,
		WorkTreeHash: liveWT,
		BaseCommit:   baseCommit,
		SyncedAt:     s.clock().UTC().Format(time.RFC3339),
	})
}

// applyResync materializes the resync into the base: when clean (the staged
// tree equals the base), it appends nothing and returns the unchanged base;
// otherwise it APPENDS a base commit (append-only, ADR-008 §6) and returns the
// new base commit id. Refs: MGIT-35
func (s *SyncService) applyResync(ctx context.Context, clean bool, local *gitref.LocalState) (string, error) {
	if clean {
		if err := s.repo.ClearStaging(); err != nil {
			return "", fmt.Errorf("sync: clear staging: %w", err)
		}
		head, err := s.repo.Head()
		if err != nil {
			return "", fmt.Errorf("sync: resolve base: %w", err)
		}
		return head, nil
	}
	c := &model.Commit{
		AgentID: "mgit-sync",
		Message: fmt.Sprintf("[mgit-sync] resync base to local working state (git %s)", short(local.HeadCommit)),
	}
	hash, err := s.commitStore.CreateCommit(ctx, c)
	if err != nil {
		return "", fmt.Errorf("sync: append base commit: %w", err)
	}
	return hash, nil
}

// short returns a 12-char prefix of a commit id for log messages.
func short(id string) string {
	if len(id) <= 12 {
		return id
	}
	return id[:12]
}
