package service

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"

	"github.com/hyper-swe/mgit/internal/model"
	gitstore "github.com/hyper-swe/mgit/internal/store/git"
	"github.com/hyper-swe/mgit/internal/store/index"
)

// RollbackRequest holds parameters for rolling back task commits.
// Refs: FR-6
type RollbackRequest struct {
	TaskID string `json:"task_id"`
	Reason string `json:"reason,omitempty"`
	DryRun bool   `json:"dry_run,omitempty"`
}

// RollbackService creates revert commits that undo a task's net changes.
// The revert RESTORES CONTENT — in the new commit's tree and in the working
// directory — not just a record of intent (MGIT-54). Original commits are
// never deleted: append-only per FR-12.
// Refs: FR-6, FR-12, MGIT-3.1.3, MGIT-54
type RollbackService struct {
	commitStore *gitstore.CommitStore
	diffStore   *gitstore.DiffStore
	indexStore  *index.Store
	repo        *gitstore.Repository
	audit       *AuditService
}

// NewRollbackService creates a RollbackService with injected dependencies.
func NewRollbackService(repo *gitstore.Repository, cs *gitstore.CommitStore, idx *index.Store) *RollbackService {
	return &RollbackService{
		commitStore: cs,
		diffStore:   gitstore.NewDiffStore(repo),
		indexStore:  idx,
		repo:        repo,
	}
}

// WithAudit attaches an AuditService so successful rollbacks are recorded in the
// append-only audit trail surfaced by `mgit audit`. Returns the receiver for
// fluent wiring. If unset, rollbacks proceed without an audit entry.
// Refs: FR-12, MGIT-20
func (s *RollbackService) WithAudit(a *AuditService) *RollbackService {
	s.audit = a
	return s
}

// RollbackTask creates a revert commit that undoes the task's NET changes and
// materializes the restored state into the working directory. Semantics
// (MGIT-54):
//
//   - Net, not replayed: the task's commits are folded into one before→after
//     state per path; the revert restores the before state.
//   - Ancestors only: commits not on the current branch lineage (e.g. the
//     task's squash artifact on task/<id>) are excluded from the net.
//   - Conflict-safe: a path changed since the task (another task's commit) or
//     carrying uncommitted local state refuses with ErrRollbackConflict —
//     rollback never clobbers other work.
//   - Append-only: originals remain; the revert is a new commit (FR-12).
//   - Net-empty (task already reverted or self-canceling) is an error and
//     mints no commit.
//
// If DryRun is true, returns the would-be revert (with its inverse diffs)
// without changing anything.
// Refs: FR-6, FR-12, MGIT-54
func (s *RollbackService) RollbackTask(ctx context.Context, req RollbackRequest) (*model.Commit, error) {
	records, err := s.indexStore.GetTaskCommits(ctx, req.TaskID)
	if err != nil {
		return nil, fmt.Errorf("rollback task %s: get commits: %w", req.TaskID, err)
	}
	if len(records) == 0 {
		return nil, fmt.Errorf("rollback task %s: %w", req.TaskID, model.ErrTaskNotFound)
	}

	inverseDiffs, err := s.taskInverseDiffs(ctx, req.TaskID, records)
	if err != nil {
		return nil, err
	}
	if len(inverseDiffs) == 0 {
		return nil, fmt.Errorf("rollback task %s: no net content or mode change to revert "+
			"(already rolled back, or the task's commits cancel out)", req.TaskID)
	}

	// Refuse to clobber uncommitted local state on any affected path. Runs for
	// dry runs too, so a dry run predicts what the real run would do.
	paths := make([]string, 0, len(inverseDiffs))
	for _, d := range inverseDiffs {
		paths = append(paths, d.Path)
	}
	dirty, err := s.repo.DirtyPaths(paths)
	if err != nil {
		return nil, fmt.Errorf("rollback task %s: %w", req.TaskID, err)
	}
	if len(dirty) > 0 {
		return nil, fmt.Errorf("rollback task %s: %w: uncommitted local changes on %s "+
			"(commit or restore them first)", req.TaskID, model.ErrRollbackConflict, strings.Join(dirty, ", "))
	}

	reason := req.Reason
	if reason == "" {
		reason = "rollback"
	}
	taskID, err := model.ParseTaskID(req.TaskID)
	if err != nil {
		return nil, fmt.Errorf("rollback task: %w", err)
	}
	revertCommit := &model.Commit{
		TaskID:     taskID,
		AgentID:    "mgit-rollback",
		Message:    fmt.Sprintf("[MGIT:%s] Revert: %s (%d commits)", req.TaskID, reason, len(records)),
		FileDiffs:  inverseDiffs,
		CommitType: model.CommitTypeRollback,
		CreatedBy:  "mgit-rollback",
		Branch:     model.TaskBranchName(req.TaskID),
	}

	if req.DryRun {
		return revertCommit, nil
	}

	// Create the revert commit with the RESTORED tree, restore the working
	// directory, and index it — crash-journaled so an interruption between the
	// ref advance and the disk/index writes is completed on the next open
	// instead of leaving tip and disk divergent (which the auto-resync would
	// otherwise absorb, silently undoing the revert). A content conflict from
	// the store means the tree moved past the task.
	hash, err := s.commitStore.CreateCommitFromDiffsJournaled(ctx, revertCommit, inverseDiffs, gitstore.ApplyIndexEntry{
		TaskID: req.TaskID, AgentID: "mgit-rollback",
	})
	if err != nil {
		if errors.Is(err, model.ErrContentConflict) {
			return nil, fmt.Errorf("rollback task %s: %w: %w", req.TaskID, model.ErrRollbackConflict, err)
		}
		return nil, fmt.Errorf("rollback task %s: %w", req.TaskID, err)
	}

	position := len(records)
	err = s.indexStore.AddCommitToTask(ctx, req.TaskID, hash, revertCommit.ContentHash, "mgit-rollback", position)
	if err != nil {
		return nil, fmt.Errorf("rollback task %s: index revert commit: %w", req.TaskID, err)
	}
	// The revert is durable on disk and indexed: clear the crash journal.
	if err := s.repo.ClearApplyJournal(); err != nil {
		return nil, fmt.Errorf("rollback task %s: clear apply journal: %w", req.TaskID, err)
	}

	// Record the operation in the append-only audit trail (MGIT-20).
	if err := s.logAudit(revertCommit, req.TaskID, reason); err != nil {
		return nil, fmt.Errorf("rollback task %s: audit: %w", req.TaskID, err)
	}

	return revertCommit, nil
}

// pathState tracks one path's before/after blob state and file mode across a
// task's commits. A nil hash means "absent". Refs: MGIT-54
type pathState struct {
	before     *string
	after      *string
	beforeMode model.FileDiffMode
	afterMode  model.FileDiffMode
}

// taskInverseDiffs folds the task's on-lineage commits into a net
// before→after state per path (from REAL tree diffs, not the unpersisted
// FileDiffs field) and returns the INVERSE diffs that restore the before
// state (hash and mode: the From-side OldMode makes type changes and chmods
// revertible). The fold chain-verifies against interleaved commits (see
// foldDiffs). Refs: MGIT-54
func (s *RollbackService) taskInverseDiffs(ctx context.Context, taskID string, records []index.CommitRecord) ([]model.FileDiff, error) {
	states := map[string]*pathState{}

	for _, rec := range records {
		onLineage, err := s.repo.IsAncestorOfHead(rec.CommitHash)
		if err != nil {
			return nil, fmt.Errorf("rollback task %s: %w", taskID, err)
		}
		if !onLineage {
			continue // e.g. the squash artifact on task/<id>
		}
		c, err := s.commitStore.GetCommit(ctx, rec.CommitHash)
		if err != nil {
			return nil, fmt.Errorf("rollback task %s: get commit %s: %w", taskID, rec.CommitHash, err)
		}
		if c.ParentID == "" {
			return nil, fmt.Errorf("rollback task %s: commit %s has no parent (cannot diff the initial commit)", taskID, rec.CommitHash)
		}
		diffs, err := s.diffStore.DiffCommits(ctx, c.ParentID, c.CommitID)
		if err != nil {
			return nil, fmt.Errorf("rollback task %s: diff %s: %w", taskID, rec.CommitHash, err)
		}
		if err := foldDiffs(states, diffs); err != nil {
			return nil, fmt.Errorf("rollback task %s: %w", taskID, err)
		}
	}

	return inverseFromStates(states), nil
}

// foldDiffs folds one commit's diffs into the running per-path states: the
// first touch records the before state (hash AND mode, from the diff's From
// side); every touch updates the after state. It CHAIN-VERIFIES each diff's
// old state against the fold's running after state: a mismatch means another
// commit touched the path BETWEEN this task's commits, and reverting the net
// would silently destroy that interleaved work — refuse with a conflict
// instead. Refs: MGIT-54
func foldDiffs(states map[string]*pathState, diffs []model.FileDiff) error {
	for _, d := range diffs {
		st, seen := states[d.Path]
		if !seen {
			st = &pathState{beforeMode: d.OldMode}
			if d.Operation != model.DiffAdded {
				old := d.OldHash
				st.before = &old
			}
			states[d.Path] = st
		} else if err := verifyFoldChain(st, d); err != nil {
			return err
		}
		if d.Operation == model.DiffDeleted {
			st.after = nil
		} else {
			nh := d.NewHash
			st.after = &nh
			st.afterMode = d.Mode
		}
	}
	return nil
}

// verifyFoldChain checks a later task commit's recorded old state matches the
// fold's running after state for the path. A break means an interleaved
// commit (another task, or a base resync) changed the path between this
// task's commits. Refs: MGIT-54 (review finding H1)
func verifyFoldChain(st *pathState, d model.FileDiff) error {
	if d.Operation == model.DiffAdded {
		if st.after != nil {
			return fmt.Errorf("%w: %s was re-added while already present "+
				"(an interleaved commit changed it between this task's commits)",
				model.ErrRollbackConflict, d.Path)
		}
		return nil
	}
	if st.after == nil || *st.after != d.OldHash || st.afterMode != d.OldMode {
		return fmt.Errorf("%w: %s changed between this task's commits "+
			"(an interleaved commit touched it; reverting the net would destroy that work)",
			model.ErrRollbackConflict, d.Path)
	}
	return nil
}

// inverseFromStates converts net before/after states into the inverse diffs
// that restore each path's before state — hash AND file mode, so a type
// change (regular↔symlink) or a mode-only chmod is reverted faithfully —
// sorted by path for determinism. Paths whose net state (hash and mode) is
// unchanged produce nothing. Refs: MGIT-54 (review findings H2, M4)
func inverseFromStates(states map[string]*pathState) []model.FileDiff {
	paths := make([]string, 0, len(states))
	for p := range states {
		paths = append(paths, p)
	}
	sort.Strings(paths)

	var inverse []model.FileDiff
	for _, p := range paths {
		st := states[p]
		switch {
		case st.before == nil && st.after != nil:
			inverse = append(inverse, model.FileDiff{
				Path: p, Operation: model.DiffDeleted, OldHash: *st.after,
			})
		case st.before != nil && st.after == nil:
			inverse = append(inverse, model.FileDiff{
				Path: p, Operation: model.DiffAdded, NewHash: *st.before, Mode: st.beforeMode,
			})
		case st.before != nil && (*st.before != *st.after || st.beforeMode != st.afterMode):
			inverse = append(inverse, model.FileDiff{
				Path: p, Operation: model.DiffModified,
				OldHash: *st.after, NewHash: *st.before,
				Mode: st.beforeMode, OldMode: st.afterMode,
			})
		}
	}
	return inverse
}

// logAudit appends a ROLLBACK entry to the audit trail. No-op when no
// AuditService is wired. Refs: FR-12, MGIT-20
func (s *RollbackService) logAudit(c *model.Commit, taskID, reason string) error {
	if s.audit == nil {
		return nil
	}
	return s.audit.LogOperation(AuditEntry{
		Operation: AuditRollback,
		AgentID:   c.AgentID,
		TaskID:    taskID,
		CommitID:  c.CommitID,
		Details:   reason,
	})
}
