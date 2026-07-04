package git

import (
	"context"
	"fmt"
	"path/filepath"

	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/object"

	"github.com/hyper-swe/mgit/internal/model"
)

// CreateCommitFromDiffs creates a commit whose tree is the CURRENT branch
// tip's tree with the given file diffs applied — the content-carrying
// primitive behind rollback and cherry-pick (MGIT-54). It differs from
// CreateCommit in two deliberate ways:
//
//   - The tree comes from the diffs, NOT from the CLI staging area, and the
//     staging area is neither consumed nor cleared: a revert/pick must not
//     swallow the agent's staged work-in-progress.
//   - Every diff is VERIFIED against the parent tree before applying: a
//     modified/deleted path must still be at the diff's OldHash, and an added
//     path must be absent (re-adding an identical blob is a no-op). A mismatch
//     fails with ErrContentConflict naming the path — never a silent clobber.
//
// The referenced blobs must already exist in the object store, which holds for
// diffs derived from history (DiffStore.DiffCommits). Append-only: the parent
// and all prior commits remain; the branch tip advances via compare-and-set.
// Refs: MGIT-54, FR-6, FR-12
func (cs *CommitStore) CreateCommitFromDiffs(ctx context.Context, c *model.Commit, diffs []model.FileDiff) (string, error) {
	return cs.createFromDiffs(ctx, c, diffs, nil)
}

// CreateCommitFromDiffsJournaled is CreateCommitFromDiffs plus the crash
// journal and working-directory materialization: the journal is written
// after the commit object exists but BEFORE the ref advances, and the
// applied diffs are materialized to disk after. The CALLER indexes the
// commit and then clears the journal (Repository.ClearApplyJournal); until
// then an interrupted process is completed by recovery on the next open.
// Refs: MGIT-54 (review finding H3)
func (cs *CommitStore) CreateCommitFromDiffsJournaled(ctx context.Context, c *model.Commit, diffs []model.FileDiff, entry ApplyIndexEntry) (string, error) {
	hash, err := cs.createFromDiffs(ctx, c, diffs, func() error {
		return cs.repo.WriteApplyJournal(ApplyJournal{
			Root:        cs.repo.Root(),
			CommitHash:  c.CommitID,
			ContentHash: c.ContentHash,
			Index:       entry,
			Diffs:       diffs,
		})
	})
	if err != nil {
		return "", err
	}
	if err := cs.repo.MaterializeDiffs(diffs); err != nil {
		return "", fmt.Errorf("apply to working directory: %w "+
			"(a recovery journal is pending; the next mgit command completes the application)", err)
	}
	return hash, nil
}

// createFromDiffs is the shared body: verify + build tree, write the commit
// object, run the optional preAdvance hook (the journal write) while the
// commit is still unreferenced, then CAS-advance the ref. Refs: MGIT-54
func (cs *CommitStore) createFromDiffs(_ context.Context, c *model.Commit, diffs []model.FileDiff, preAdvance func() error) (string, error) {
	if len(diffs) == 0 {
		return "", fmt.Errorf("create commit from diffs: empty diff set")
	}

	goRepo := cs.repo.repo
	c.CreatedAt = cs.repo.Now()

	headRef, err := cs.repo.currentRef()
	if err != nil {
		return "", fmt.Errorf("resolve HEAD: %w", err)
	}
	parentHash := headRef.Hash()
	c.ParentID = parentHash.String()

	files, err := cs.repo.headFiles()
	if err != nil {
		return "", err
	}

	for _, d := range diffs {
		if err := applyVerifiedDiff(files, d); err != nil {
			return "", err
		}
	}

	treeHash, err := writeNestedTree(goRepo.Storer, files)
	if err != nil {
		return "", err
	}
	c.TreeHash = treeHash.String()

	commitHash, err := writeCommit(goRepo.Storer, commitParams{
		tree:    treeHash,
		parents: []plumbing.Hash{parentHash},
		message: c.Message,
		authorAt: object.Signature{
			Name:  c.AgentID,
			Email: c.AgentID + "@mgit",
			When:  c.CreatedAt,
		},
	})
	if err != nil {
		return "", fmt.Errorf("create commit from diffs: %w", err)
	}

	c.CommitID = commitHash.String()
	c.ContentHash = c.ComputeContentHash()

	// The commit object exists but is unreferenced: journal now, so a crash
	// on either side of the ref advance is recoverable (an unreferenced
	// commit with a journal is discarded; a referenced one is completed).
	if preAdvance != nil {
		if err := preAdvance(); err != nil {
			return "", err
		}
	}

	if err := advanceBranchRefCAS(goRepo.Storer, headRef.Name(), commitHash, parentHash); err != nil {
		return "", fmt.Errorf("update ref: %w", err)
	}
	// Deliberately NO clearStaging here (see doc comment).
	return c.CommitID, nil
}

// applyVerifiedDiff applies one diff onto the flattened tree map after
// verifying the tree's current state matches the diff's expected old state.
// Refs: MGIT-54
func applyVerifiedDiff(files map[string]blobEntry, d model.FileDiff) error {
	path := filepath.ToSlash(d.Path)
	existing, exists := files[path]

	switch d.Operation {
	case model.DiffAdded:
		if exists {
			if existing.hash.String() == d.NewHash {
				return nil // idempotent re-add of the identical blob
			}
			return fmt.Errorf("%w: %s already exists with different content", model.ErrContentConflict, path)
		}
		files[path] = blobEntry{hash: plumbing.NewHash(d.NewHash), mode: gitModeFromDiff(d.Mode)}
	case model.DiffDeleted:
		if err := verifyOldState(path, existing, exists, d.OldHash); err != nil {
			return err
		}
		delete(files, path)
	default: // modified
		if err := verifyOldState(path, existing, exists, d.OldHash); err != nil {
			return err
		}
		files[path] = blobEntry{hash: plumbing.NewHash(d.NewHash), mode: gitModeFromDiff(d.Mode)}
	}
	return nil
}

// verifyOldState checks a modify/delete target is present and still at the
// diff's recorded old blob. Refs: MGIT-54
func verifyOldState(path string, existing blobEntry, exists bool, oldHash string) error {
	if !exists {
		return fmt.Errorf("%w: %s no longer exists", model.ErrContentConflict, path)
	}
	if existing.hash.String() != oldHash {
		return fmt.Errorf("%w: %s changed since the diff was computed", model.ErrContentConflict, path)
	}
	return nil
}
