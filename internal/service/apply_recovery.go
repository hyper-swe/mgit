package service

import (
	"context"
	"fmt"

	gitstore "github.com/hyper-swe/mgit/internal/store/git"
	"github.com/hyper-swe/mgit/internal/store/index"
)

// RecoverPendingApply completes a content-applying commit (rollback,
// cherry-pick — MGIT-54) that was interrupted between its ref advance and
// its working-directory/index writes. It runs at every app open, under the
// process lock, and is a no-op when no journal is pending or the journal
// belongs to a different root (that root's next open recovers it).
//
// Outcomes by journal state:
//   - recorded commit IS the branch tip: re-materialize the diffs (idempotent
//     writes) and index the commit if missing, then clear — the application
//     is completed, so the auto-resync can never absorb the stale disk state
//     and silently undo a revert.
//   - recorded commit is an OLDER ancestor: later commits govern the working
//     tree; index it if missing and clear, without touching disk.
//   - recorded commit never became the tip (crash before the ref advance):
//     nothing was applied; clear the journal (the unreferenced commit object
//     is inert).
//
// Refs: MGIT-54 (review finding H3), FR-6, FR-12
func RecoverPendingApply(ctx context.Context, repo *gitstore.Repository, cs *gitstore.CommitStore, idx *index.Store) error {
	j, found, err := repo.ReadApplyJournal()
	if err != nil {
		return fmt.Errorf("apply recovery: %w", err)
	}
	if !found {
		return nil
	}
	if j.Root != repo.Root() {
		return nil // another worktree's pending apply; recovered from there.
	}

	head, err := repo.Head()
	if err != nil {
		return fmt.Errorf("apply recovery: resolve HEAD: %w", err)
	}

	switch head {
	case j.CommitHash:
		if err := repo.MaterializeDiffs(j.Diffs); err != nil {
			return fmt.Errorf("apply recovery: materialize: %w", err)
		}
		if err := indexIfMissing(ctx, idx, j); err != nil {
			return err
		}
	default:
		onLineage, ancErr := repo.IsAncestorOfHead(j.CommitHash)
		if ancErr != nil {
			return fmt.Errorf("apply recovery: %w", ancErr)
		}
		if onLineage {
			// Later commits govern the tree; only the index entry may be owed.
			if err := indexIfMissing(ctx, idx, j); err != nil {
				return err
			}
		}
		// Not on the lineage: the ref never advanced; nothing to complete.
	}
	if err := repo.ClearApplyJournal(); err != nil {
		return fmt.Errorf("apply recovery: %w", err)
	}
	return nil
}

// indexIfMissing adds the journaled commit to its task's index unless a
// record for that hash already exists (the crash may have happened after
// indexing but before the journal cleared). Refs: MGIT-54
func indexIfMissing(ctx context.Context, idx *index.Store, j gitstore.ApplyJournal) error {
	records, err := idx.GetTaskCommits(ctx, j.Index.TaskID)
	if err != nil {
		return fmt.Errorf("apply recovery: read task index: %w", err)
	}
	for _, r := range records {
		if r.CommitHash == j.CommitHash {
			return nil
		}
	}
	if err := idx.AddCommitToTask(ctx, j.Index.TaskID, j.CommitHash, j.ContentHash, j.Index.AgentID, len(records)); err != nil {
		return fmt.Errorf("apply recovery: index commit: %w", err)
	}
	return nil
}
