package service

import (
	"context"
	"fmt"
	"strings"

	"github.com/hyper-swe/mgit/internal/model"
	gitstore "github.com/hyper-swe/mgit/internal/store/git"
)

// CherryPickRequest holds parameters for applying one commit's changes onto
// the current branch. Refs: FR-8.16, MGIT-54
type CherryPickRequest struct {
	// SourceHash is the commit (full or short hash) whose changes to apply.
	SourceHash string `json:"source_hash"`
	// TaskID optionally re-attributes the new commit; default is the source
	// commit's provenance (its [MGIT:] tag, MGIT-19).
	TaskID string `json:"task_id,omitempty"`
}

// CherryPick applies the source commit's changes (its tree diff against its
// parent) onto the current branch as a new commit, MATERIALIZING the content
// in the new tree and the working directory (MGIT-54) — previously the pick
// was a provenance record whose diffs never reached the tree. Conflict-safe:
// a target path that diverged from the pick's recorded old state, or that
// carries uncommitted local changes, refuses with ErrContentConflict.
// Append-only: the source commit is untouched.
// Refs: FR-8.16, FR-12, MGIT-54, MGIT-19
func (s *CommitService) CherryPick(ctx context.Context, req CherryPickRequest) (*model.Commit, error) {
	source, err := s.commitStore.GetCommit(ctx, req.SourceHash)
	if err != nil {
		return nil, fmt.Errorf("cherry-pick: %w", err)
	}

	pickTask := req.TaskID
	if pickTask == "" {
		pickTask = source.TaskID.String()
	}
	if pickTask == "" {
		return nil, fmt.Errorf("cherry-pick: source commit %s has no task ID; pass --task-id", source.ShortID())
	}
	taskID, err := model.ParseTaskID(pickTask)
	if err != nil {
		return nil, fmt.Errorf("cherry-pick: %w", err)
	}

	if source.ParentID == "" {
		return nil, fmt.Errorf("cherry-pick: commit %s has no parent (cannot diff the initial commit)", source.ShortID())
	}
	diffs, err := gitstore.NewDiffStore(s.repo).DiffCommits(ctx, source.ParentID, source.CommitID)
	if err != nil {
		return nil, fmt.Errorf("cherry-pick: diff source: %w", err)
	}
	if len(diffs) == 0 {
		return nil, fmt.Errorf("cherry-pick: commit %s carries no content change", source.ShortID())
	}

	// Refuse to clobber uncommitted local state on any affected path.
	paths := make([]string, 0, len(diffs))
	for _, d := range diffs {
		paths = append(paths, d.Path)
	}
	dirty, err := s.repo.DirtyPaths(paths)
	if err != nil {
		return nil, fmt.Errorf("cherry-pick: %w", err)
	}
	if len(dirty) > 0 {
		return nil, fmt.Errorf("cherry-pick: %w: uncommitted local changes on %s "+
			"(commit or restore them first)", model.ErrContentConflict, strings.Join(dirty, ", "))
	}

	pick := &model.Commit{
		TaskID:     taskID,
		AgentID:    "mgit-cherry-pick",
		Message:    fmt.Sprintf("[MGIT:%s] cherry-pick %s: %s", pickTask, source.ShortID(), strings.TrimSpace(source.Message)),
		FileDiffs:  diffs,
		CommitType: model.CommitTypeNormal,
		CreatedBy:  "mgit-cherry-pick",
		Branch:     model.TaskBranchName(pickTask),
	}

	// Crash-journaled: an interruption between the ref advance and the
	// disk/index writes is completed on the next open (MGIT-54 H3).
	hash, err := s.commitStore.CreateCommitFromDiffsJournaled(ctx, pick, diffs, gitstore.ApplyIndexEntry{
		TaskID: pickTask, AgentID: pick.AgentID,
	})
	if err != nil {
		return nil, fmt.Errorf("cherry-pick %s: %w", source.ShortID(), err)
	}

	existing, err := s.indexStore.GetTaskCommits(ctx, pickTask)
	if err != nil {
		return nil, fmt.Errorf("cherry-pick: get existing commits: %w", err)
	}
	err = s.indexStore.AddCommitToTask(ctx, pickTask, hash, pick.ContentHash, pick.AgentID, len(existing))
	if err != nil {
		return nil, fmt.Errorf("cherry-pick: index commit: %w", err)
	}
	// Durable on disk and indexed: clear the crash journal.
	if err := s.repo.ClearApplyJournal(); err != nil {
		return nil, fmt.Errorf("cherry-pick: clear apply journal: %w", err)
	}

	if err := s.logAudit(pick); err != nil {
		return nil, fmt.Errorf("cherry-pick: audit: %w", err)
	}
	return pick, nil
}
