package service

import (
	"context"
	"fmt"
	"os"
	"time"

	"github.com/hyper-swe/mgit/internal/model"
	gitstore "github.com/hyper-swe/mgit/internal/store/git"
	"github.com/hyper-swe/mgit/internal/store/index"
)

// WorktreeService orchestrates worktree lifecycle with task binding.
// Wraps the SQLite worktree registry and coordinates with BranchService.
// Refs: FR-16, MGIT-8.2.1
type WorktreeService struct {
	indexStore *index.Store
	branch     *BranchService
	worktree   *gitstore.WorktreeStore
	repo       *gitstore.Repository
	commits    *gitstore.CommitStore
	sync       *SyncService
	clock      func() time.Time
}

// NewWorktreeService creates a WorktreeService with injected dependencies. The
// WorktreeStore is used to materialize a new worktree's branch source onto disk
// (MGIT-17).
func NewWorktreeService(idx *index.Store, branch *BranchService, wt *gitstore.WorktreeStore, clock func() time.Time) *WorktreeService {
	return &WorktreeService{
		indexStore: idx,
		branch:     branch,
		worktree:   wt,
		clock:      clock,
	}
}

// WithSync attaches the auto-housekeeping dependencies (ADR-008, MGIT-35): the
// SyncService resyncs the `.mgit` base from the current local working state
// BEFORE a new worktree is forked (so it carries the developer's unpushed
// foundation), and repo/commits resolve+pin the per-task fork-base. Returns the
// receiver for fluent wiring. When unset, Add behaves as before (no auto-sync,
// empty fork-base) so legacy callers/tests keep working. Refs: MGIT-35
func (s *WorktreeService) WithSync(sync *SyncService, repo *gitstore.Repository, cs *gitstore.CommitStore) *WorktreeService {
	s.sync = sync
	s.repo = repo
	s.commits = cs
	return s
}

// Add creates a new worktree bound to a task.
// Auto-creates the task branch if it doesn't exist.
// Refs: FR-16.1, FR-16.2
func (s *WorktreeService) Add(ctx context.Context, opts model.WorktreeAddOptions) (*model.WorktreeInfo, error) {
	if err := opts.Validate(); err != nil {
		return nil, fmt.Errorf("worktree add: %w", err)
	}

	// Derive branch name from task if not provided
	branchName := opts.Branch
	if branchName == "" {
		branchName = "task/" + opts.TaskID
	}

	// Auto-housekeep BEFORE forking so the new worktree carries the current
	// local working state (incl. the developer's unpushed foundation) — the
	// concrete advantage of an mgit worktree over a git worktree (ADR-008 §2).
	// Then resolve+pin the fork-base so a later base resync cannot shift it.
	forkBase, err := s.resolveForkBase(ctx, opts, branchName)
	if err != nil {
		return nil, fmt.Errorf("worktree add: %w", err)
	}

	wt := &model.WorktreeInfo{
		Path:      opts.Path,
		Name:      model.DeriveNameFromPath(opts.Path),
		Branch:    branchName,
		TaskID:    opts.TaskID,
		AgentID:   opts.AgentID,
		ForkBase:  forkBase,
		CreatedAt: s.clock(),
	}

	// Register in SQLite (UNIQUE constraints enforce isolation) BEFORE touching
	// disk, so a duplicate path/task is rejected without materializing anything.
	if err := s.indexStore.InsertWorktree(ctx, wt); err != nil {
		return nil, fmt.Errorf("worktree add: %w", err)
	}

	// Materialize the branch's source into the linked worktree path so it is a
	// usable working copy, not an empty dir (MGIT-17). On failure, roll back the
	// registration so we never leave a registered-but-empty worktree behind.
	if err := s.worktree.MaterializeBranchTo(ctx, branchName, opts.Path); err != nil {
		_ = s.indexStore.DeleteWorktree(ctx, opts.Path)
		return nil, fmt.Errorf("worktree add: materialize source: %w", err)
	}

	// Write the linked-worktree marker (with the pinned fork-base) so `mgit` run
	// from inside the worktree binds to the shared parent store on this task
	// branch and computes diff/squash against the pinned base (ADR-007, ADR-008).
	if err := s.worktree.WriteWorktreeMarkerWithBase(opts.Path, branchName, opts.TaskID, forkBase); err != nil {
		_ = s.indexStore.DeleteWorktree(ctx, opts.Path)
		return nil, fmt.Errorf("worktree add: write marker: %w", err)
	}

	return wt, nil
}

// resolveForkBase ensures the base is synced (unless an explicit --base is
// given) and returns the commit the task branch is/forked at, auto-creating the
// branch at that pinned base when it does not yet exist. With --base, the branch
// is pinned to the resolved ref; without it, the branch forks off the
// auto-resynced local base. An existing branch keeps its current tip as the
// pinned base. Refs: MGIT-35, ADR-008 §2,§4
func (s *WorktreeService) resolveForkBase(ctx context.Context, opts model.WorktreeAddOptions, branchName string) (string, error) {
	if existing, err := s.branch.GetBranch(ctx, branchName); err == nil {
		return existing.HeadCommit, nil
	}
	if opts.Base != "" {
		return s.createBranchAtBase(ctx, opts.TaskID, opts.Base)
	}
	if s.sync != nil {
		if err := s.sync.EnsureSynced(ctx); err != nil {
			return "", fmt.Errorf("auto-resync base: %w", err)
		}
	}
	br, err := s.branch.CreateBranch(ctx, opts.TaskID)
	if err != nil {
		return "", fmt.Errorf("create branch: %w", err)
	}
	return br.HeadCommit, nil
}

// createBranchAtBase resolves the explicit --base ref to a concrete commit and
// forks the task branch there, pinning it. Refs: MGIT-35, ADR-008 §4
func (s *WorktreeService) createBranchAtBase(ctx context.Context, taskID, baseRef string) (string, error) {
	if s.commits == nil {
		return "", fmt.Errorf("--base requires sync wiring")
	}
	commit, err := s.resolveBaseCommit(ctx, baseRef)
	if err != nil {
		return "", err
	}
	br, err := s.branch.CreateBranchAt(ctx, taskID, commit)
	if err != nil {
		return "", fmt.Errorf("create branch at base: %w", err)
	}
	return br.HeadCommit, nil
}

// resolveBaseCommit resolves an `mgit work --base <ref>` argument to a concrete
// mgit commit id. It accepts (in order) an mgit commit hash (full/abbreviated
// hex), the literal "HEAD", or an mgit branch name (e.g. "main", "task/X"), so
// the natural `--base main` works rather than requiring a raw SHA. Refs that
// exist only in the project's git (e.g. a git tag) are out of scope for v1 — a
// documented follow-up. Refs: MGIT-35, ADR-008 §4
func (s *WorktreeService) resolveBaseCommit(ctx context.Context, baseRef string) (string, error) {
	if baseRef == "HEAD" {
		head, err := s.repo.Head()
		if err != nil {
			return "", fmt.Errorf("resolve --base HEAD: %w", err)
		}
		return head, nil
	}
	if c, err := s.commits.GetCommit(ctx, baseRef); err == nil {
		return c.CommitID, nil
	}
	if br, err := s.branch.GetBranch(ctx, baseRef); err == nil && br.HeadCommit != "" {
		return br.HeadCommit, nil
	}
	return "", fmt.Errorf("resolve --base %q: not a known mgit commit, branch, or HEAD", baseRef)
}

// List returns all registered worktrees.
// Refs: FR-16
func (s *WorktreeService) List(ctx context.Context) ([]model.WorktreeInfo, error) {
	return s.indexStore.ListWorktrees(ctx)
}

// Remove deletes a worktree registration.
// Refs: FR-16
func (s *WorktreeService) Remove(ctx context.Context, path string, _ bool) error {
	return s.indexStore.DeleteWorktree(ctx, path)
}

// Resolve looks up a worktree by path.
// Refs: FR-16
func (s *WorktreeService) Resolve(ctx context.Context, path string) (*model.WorktreeInfo, error) {
	return s.indexStore.GetWorktree(ctx, path)
}

// Prune removes worktree registrations that are stale. A worktree is
// considered stale when:
//   - its filesystem path no longer exists, OR
//   - its created_at is older than `staleAfter` (when staleAfter > 0).
//
// When dryRun is true, the matching paths are returned without being
// deleted from the registry.
// Refs: FR-16, MGIT-8.2.2
func (s *WorktreeService) Prune(ctx context.Context, dryRun bool, staleAfter time.Duration) ([]string, error) {
	all, err := s.List(ctx)
	if err != nil {
		return nil, fmt.Errorf("worktree prune: list: %w", err)
	}

	now := s.clock()
	var stale []string
	for _, wt := range all {
		isStale := false
		if _, statErr := os.Stat(wt.Path); statErr != nil && os.IsNotExist(statErr) {
			isStale = true
		}
		if !isStale && staleAfter > 0 && !wt.CreatedAt.IsZero() {
			if now.Sub(wt.CreatedAt) > staleAfter {
				isStale = true
			}
		}
		if isStale {
			stale = append(stale, wt.Path)
		}
	}

	if dryRun {
		return stale, nil
	}

	for _, path := range stale {
		if err := s.indexStore.DeleteWorktree(ctx, path); err != nil {
			return nil, fmt.Errorf("worktree prune: delete %s: %w", path, err)
		}
	}
	return stale, nil
}
