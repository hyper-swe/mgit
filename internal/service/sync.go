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

// committedReader reads the project's git-COMMITTED file set (project-relative
// path -> git blob id) READ-ONLY. It is an interface so the read-safe resync is
// unit-testable without a real `.git`. Refs: MGIT-123, ADR-008 §3
type committedReader func(projectRoot string) (map[string]string, error)

// SyncService self-heals the `.mgit` base so a NEW worktree always carries the
// project's current local working state, eliminating the manual
// `mgit add . && mgit commit` resync. It runs a CHEAP content-based drift gate
// before every base-dependent command and resyncs only on real drift,
// transactionally, under the caller's already-held store lock. Pinned per-task
// fork-bases are never touched — only the shared base branch advances.
// Refs: MGIT-35, ADR-008 §3,§4,§6
type SyncService struct {
	repo          *gitstore.Repository
	worktree      *gitstore.WorktreeStore
	commitStore   *gitstore.CommitStore
	readLocal     localStateReader
	readCommitted committedReader
	clock         func() time.Time
	// boundTask is non-empty when the App is a linked worktree; a worktree has a
	// pinned fork-base and must NEVER resync (ADR-008 §3). Refs: MGIT-35
	boundTask string
}

// NewSyncService creates a SyncService. boundTask is the App's BoundTask (empty
// for a normal store, non-empty inside a linked worktree). Refs: MGIT-35
func NewSyncService(repo *gitstore.Repository, ws *gitstore.WorktreeStore, cs *gitstore.CommitStore,
	boundTask string, clock func() time.Time) *SyncService {
	return &SyncService{
		repo:          repo,
		worktree:      ws,
		commitStore:   cs,
		readLocal:     gitref.ReadLocalState,
		readCommitted: gitref.CommittedBlobs,
		clock:         clock,
		boundTask:     boundTask,
	}
}

// withLocalReader overrides the git-state reader (test seam). Refs: MGIT-35
func (s *SyncService) withLocalReader(r localStateReader) *SyncService {
	s.readLocal = r
	return s
}

// withCommittedReader overrides the git-committed-tree reader (test seam).
// Refs: MGIT-123
func (s *SyncService) withCommittedReader(r committedReader) *SyncService {
	s.readCommitted = r
	return s
}

// EnsureSynced is the READ-SAFE auto-housekeeping gate, used by the read verbs
// (`mgit status`, `mgit diff`, the MCP status tool). It keeps the base coherent
// with what the user's git has COMMITTED and NEVER absorbs uncommitted
// working-tree content into the base.
//
// That boundary is the MGIT-123 fix. A read verb that absorbs the working tree
// makes the user's edit part of the base, so `status` reports clean (it just
// made it so) and the edit becomes uncommittable to any task — silently landing
// a user's change in an untagged `[mgit-sync]` commit instead of a task-tagged
// micro-commit. A command that REPORTS state must not change the state it
// reports. Capturing uncommitted local foundation is still ADR-008 §2 behavior,
// but it belongs to the explicit new-worktree boundary
// (EnsureSyncedForNewWorktree), not to a read.
//
// The gate therefore fires only when git's committed HEAD has moved (the actual
// staleness ADR-008 §3 exists to prevent, per the MGIT-26 drift that motivated
// it), and even then absorbs only git-committed content. It is a no-op inside a
// linked worktree (pinned fork-base) and degrades — not hard-fails — when the
// project has no readable git; any other git-read failure FAILS LOUD.
// Refs: MGIT-123, MGIT-35, ADR-008 §3,§6
func (s *SyncService) EnsureSynced(ctx context.Context) error {
	local, proceed, err := s.localOrDegrade()
	if err != nil || !proceed {
		return err
	}
	stored, found, err := s.repo.ReadSyncState()
	if err != nil {
		return fmt.Errorf("sync: read state: %w", err)
	}
	if found && stored.GitHead == local.HeadCommit {
		// git's committed truth has not moved, so the base is not stale. A
		// working-tree edit alone is the user's uncommitted work and is NOT a
		// reason to advance the base. Refs: MGIT-123
		return nil
	}
	return s.resyncCommitted(ctx, local, stored, found)
}

// EnsureSyncedForNewWorktree is the FOUNDATION-capturing gate, used only when a
// new task worktree is being created (`mgit work` / `worktree add` /
// materialize). Unlike the read-safe gate it captures the full current LOCAL
// working state — including files never committed to git — so the new worktree
// carries the developer's in-progress foundation and builds (ADR-008 §2, the
// deliberate advantage over a git worktree).
//
// This is the one place that absorption is legitimate: the user has explicitly
// asked to fork a new task base from "here". Read verbs must use EnsureSynced.
// Refs: MGIT-123, MGIT-35, ADR-008 §2,§3
func (s *SyncService) EnsureSyncedForNewWorktree(ctx context.Context) error {
	local, proceed, err := s.localOrDegrade()
	if err != nil || !proceed {
		return err
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

// localOrDegrade resolves the project's git truth for a sync gate. It reports
// proceed=false (with a nil error) for the two states where syncing is a no-op
// rather than an error: inside a linked worktree (pinned fork-base, never
// resync — ADR-008 §3) and when there is nothing to sync FROM (no git, or an
// unborn/detached HEAD with no commit). Only states that could SILENTLY corrupt
// the base (shallow/sparse/unreadable) fail loud. Refs: MGIT-35, ADR-008 §3,§6
func (s *SyncService) localOrDegrade() (*gitref.LocalState, bool, error) {
	if s.boundTask != "" {
		return nil, false, nil
	}
	local, err := s.readLocal(s.repo.Root())
	if err != nil {
		if errors.Is(err, gitref.ErrNoGit) || errors.Is(err, gitref.ErrDetachedOrUnborn) {
			return nil, false, nil
		}
		return nil, false, fmt.Errorf("sync: read git state: %w", err)
	}
	return local, true, nil
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
	// MGIT-56: paths the user had ALREADY staged are pending task work, not
	// project drift — exclude their content from base absorption, or the
	// task's first commit has no delta against the base (an empty net diff,
	// losing the review surface and squash content). The base advances on the
	// project's state, never on the task's in-flight work. (H2 above restored
	// the staging SELECTION; this keeps the CONTENT out of the base too.)
	if len(stagedBefore) > 0 {
		all, snapErr := s.repo.StagedSnapshot()
		if snapErr != nil {
			return fmt.Errorf("sync: snapshot absorbed staging: %w", snapErr)
		}
		if err := s.repo.RestoreStaging(subtractPaths(all, stagedBefore)); err != nil {
			return fmt.Errorf("sync: exclude task WIP from base: %w", err)
		}
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

// resyncCommitted brings the base up to git's COMMITTED state without ever
// absorbing uncommitted working-tree content (MGIT-123). It stages the working
// tree, then narrows the staging set to only those paths whose content git has
// actually committed, and appends a base commit only if that changes the base
// tree. Everything else — a modified tracked file, an untracked file, the
// user's staged task WIP — is left OUT of the base, so `status` still reports it
// and `commit` can still attribute it to a task.
//
// It is STAGING-NEUTRAL: the user's manual staging selection is snapshotted up
// front and restored afterward, so a read verb leaves staging untouched
// (ADR-008 §3). Refs: MGIT-123, MGIT-56, ADR-008 §3,§6
func (s *SyncService) resyncCommitted(ctx context.Context, local *gitref.LocalState,
	stored gitstore.SyncState, found bool) error {
	stagedBefore, err := s.repo.StagedSnapshot()
	if err != nil {
		return fmt.Errorf("sync: snapshot staging: %w", err)
	}
	if err := s.worktree.Add(ctx, "."); err != nil {
		return fmt.Errorf("sync: stage working tree: %w", err)
	}
	absorb, err := s.absorbableCommitted(stagedBefore)
	if err != nil {
		return err
	}
	if err := s.repo.RestoreStaging(absorb); err != nil {
		return fmt.Errorf("sync: limit base to git-committed content: %w", err)
	}
	clean, err := s.commitStore.StagedTreeMatchesHead()
	if err != nil {
		return fmt.Errorf("sync: %w", err)
	}
	baseCommit, err := s.applyResync(ctx, clean, local)
	if err != nil {
		return err
	}
	if err := s.repo.RestoreStaging(stagedBefore); err != nil {
		return fmt.Errorf("sync: restore staging: %w", err)
	}
	return s.repo.WriteSyncState(gitstore.SyncState{
		GitHead: local.HeadCommit,
		// WorkTreeHash is deliberately CARRIED FORWARD, not advanced: this path
		// never absorbed the working tree, so recording the live fingerprint
		// would falsely tell the foundation gate that the working tree is
		// already in the base and suppress the next `mgit work` capture
		// (ADR-008 §2). Refs: MGIT-123
		WorkTreeHash: carriedWorkTreeHash(stored, found),
		BaseCommit:   baseCommit,
		SyncedAt:     s.clock().UTC().Format(time.RFC3339),
	})
}

// absorbableCommitted returns the currently-staged paths that the base MAY
// absorb: those whose working content matches git's committed content, minus
// anything the user had already staged (pending task work must never be
// absorbed — MGIT-56). Refs: MGIT-123, MGIT-56
func (s *SyncService) absorbableCommitted(stagedBefore []string) ([]string, error) {
	all, err := s.repo.StagedSnapshot()
	if err != nil {
		return nil, fmt.Errorf("sync: snapshot absorbed staging: %w", err)
	}
	committed, err := s.readCommitted(s.repo.Root())
	if err != nil {
		// No git, or a HEAD with no commit: git has committed NOTHING here, so
		// nothing may be absorbed. Degrade to an empty committed set rather than
		// block a read verb (ADR-008 §6). Unreadable git still fails loud.
		if !errors.Is(err, gitref.ErrNoGit) && !errors.Is(err, gitref.ErrDetachedOrUnborn) {
			return nil, fmt.Errorf("sync: read git-committed tree: %w", err)
		}
		committed = nil
	}
	matching, err := s.repo.PathsMatchingCommitted(committed, all)
	if err != nil {
		return nil, fmt.Errorf("sync: %w", err)
	}
	return subtractPaths(matching, stagedBefore), nil
}

// carriedWorkTreeHash returns the working-tree fingerprint to persist from a
// read-safe resync: the previously stored one, or empty when no state existed.
// Refs: MGIT-123
func carriedWorkTreeHash(stored gitstore.SyncState, found bool) string {
	if !found {
		return ""
	}
	return stored.WorkTreeHash
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

// subtractPaths returns the elements of all that are not in exclude,
// preserving order. Refs: MGIT-56
func subtractPaths(all, exclude []string) []string {
	ex := make(map[string]bool, len(exclude))
	for _, p := range exclude {
		ex[p] = true
	}
	var out []string
	for _, p := range all {
		if !ex[p] {
			out = append(out, p)
		}
	}
	return out
}

// short returns a 12-char prefix of a commit id for log messages.
func short(id string) string {
	if len(id) <= 12 {
		return id
	}
	return id[:12]
}
