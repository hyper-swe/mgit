package service

import (
	"context"
	"fmt"

	"github.com/hyper-swe/mgit/internal/model"
	gitstore "github.com/hyper-swe/mgit/internal/store/git"
)

// GitPatchPreview is a task's net change rendered as a git format-patch by a
// READ: no squash commit is created, nothing is indexed, nothing is audited,
// and no ref moves.
//
// Empty reports that the task's net change against its base is genuinely
// nothing (a change and its revert). That is a legitimate outcome and Patch is
// deliberately blank for it — callers must SAY so rather than print a hunk-free
// mbox, which `git apply --allow-empty` accepts and applies to nothing.
// Refs: FR-7, MGIT-112
type GitPatchPreview struct {
	// Patch is the full git format-patch (mbox header + hunks + trailer), or
	// "" when Empty is true.
	Patch string
	// Empty is true when the task's net result tree equals its base tree.
	Empty bool
	// Message is the squash message the patch describes, available even when
	// Empty so a caller can report WHAT canceled out.
	Message string
}

// PreviewGitPatch renders a task's net change as a git format-patch WITHOUT
// creating, indexing or auditing a squash commit — the read semantics
// `mgit export --format git` always intended.
//
// It computes the same net result tree that a real squash would build (shared
// with CreateSquashCommit via BuildSquashTree) and diffs it against the task's
// base through the same go-git patch encoder `mgit squash --to-git` uses, so
// the two verbs agree on hunks by construction. MGIT-112 shipped precisely
// because the export rendered from a different source — model.FileDiffs that
// only the non-dry-run path ever populated — and emitted a syntactically valid
// patch with no hunks at all.
//
// It NEVER returns a hunk-free patch for a task whose net change is non-empty:
// that combination is an internal inconsistency and is returned as an error,
// because a patch that applies cleanly and changes nothing is worse than a
// failure the caller can see. Refs: FR-7, FR-12, MGIT-112, MGIT-33, MGIT-77
func (s *SquashService) PreviewGitPatch(ctx context.Context, req SquashRequest) (*GitPatchPreview, error) {
	plan, err := s.planSquash(ctx, req)
	if err != nil {
		return nil, err
	}
	taskID, err := model.ParseTaskID(req.TaskID)
	if err != nil {
		return nil, fmt.Errorf("preview git patch: %w", err)
	}

	tree, err := s.commitStore.BuildSquashTree(ctx, plan.hashes)
	if err != nil {
		return nil, fmt.Errorf("preview git patch for task %s: %w", req.TaskID, err)
	}
	if tree.EmptyNet() {
		return &GitPatchPreview{Empty: true, Message: plan.message}, nil
	}

	body, err := gitstore.NewDiffStore(s.repo).
		PatchFromCommitToTree(ctx, tree.BaseCommit, tree.Tree)
	if err != nil {
		return nil, fmt.Errorf("preview git patch for task %s: %w", req.TaskID, err)
	}

	header := &model.Commit{
		TaskID:     taskID,
		AgentID:    "mgit-squash",
		Message:    plan.message,
		CommitType: model.CommitTypeSquash,
		CreatedAt:  s.repo.Now(),
		ParentID:   tree.BaseCommit,
		TreeHash:   tree.Tree,
	}
	patch := s.mboxHeader(header) + body + "-- \nmgit\n"

	if err := assertPatchCarriesHunks(req.TaskID, patch); err != nil {
		return nil, err
	}
	return &GitPatchPreview{Patch: patch, Message: plan.message}, nil
}

// assertPatchCarriesHunks fails loudly when a task with a non-empty net change
// rendered to a patch with no diff hunks. Such a patch is syntactically valid,
// so `git apply --allow-empty` and `git am --allow-empty` accept it, report
// success and change nothing — the caller has already moved on by the time the
// loss is noticed. An error the caller can see is strictly better.
// Refs: MGIT-112, MGIT-77
func assertPatchCarriesHunks(taskID, patch string) error {
	if PatchHasHunks(patch) {
		return nil
	}
	return fmt.Errorf(
		"export git patch for task %s: %w: the task's net change is not empty but no diff "+
			"hunks were rendered; refusing to emit a patch that would apply cleanly and "+
			"change nothing",
		taskID, model.ErrVerificationFailed)
}
