package git

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/hyper-swe/mgit/internal/model"
)

// writeAndCommit writes files into the repo root, stages everything, and
// commits under taskID, returning the commit hash.
func writeAndCommit(t *testing.T, repo *Repository, taskID string, files map[string]string) string {
	t.Helper()
	for rel, content := range files {
		p := filepath.Join(repo.Root(), rel)
		require.NoError(t, os.MkdirAll(filepath.Dir(p), 0o750))
		require.NoError(t, os.WriteFile(p, []byte(content), 0o600))
	}
	return commitAll(t, repo, taskID)
}

// TestCreateCommitFromDiffs_AppliesDiffs_ContentRestored proves the primitive
// MGIT-54 exists for: a commit whose tree is the parent tree with the given
// diffs applied — so an inverse diff actually restores prior content, unlike
// CreateCommit which builds from staging only. Refs: MGIT-54, FR-6
func TestCreateCommitFromDiffs_AppliesDiffs_ContentRestored(t *testing.T) {
	repo := initTestRepo(t)
	cs := NewCommitStore(repo)
	ds := NewDiffStore(repo)
	ctx := context.Background()

	v1 := writeAndCommit(t, repo, "MGIT-1", map[string]string{"file.txt": "good\n"})
	v2 := writeAndCommit(t, repo, "MGIT-1", map[string]string{"file.txt": "bad\n"})

	// The change that takes v2 back to v1's content.
	diffs, err := ds.DiffCommits(ctx, v2, v1)
	require.NoError(t, err)
	require.NotEmpty(t, diffs)

	c := makeTestModelCommit(t, "MGIT-1")
	c.FileDiffs = nil
	hash, err := cs.CreateCommitFromDiffs(ctx, c, diffs)
	require.NoError(t, err)

	// The new commit's tree must contain the RESTORED content.
	got, err := cs.GetFileFromCommit(ctx, hash, "file.txt")
	require.NoError(t, err)
	assert.Equal(t, "good\n", string(got), "revert commit tree must carry the restored bytes")

	// Parent is the previous tip, ref advanced, append-only (v1, v2 remain).
	assert.Equal(t, v2, c.ParentID)
	for _, h := range []string{v1, v2} {
		_, err := cs.GetCommit(ctx, h)
		assert.NoError(t, err, "original commits must remain")
	}
}

// TestCreateCommitFromDiffs_DeleteApplies_PathGone: a Deleted diff removes the
// path from the new tree. Refs: MGIT-54
func TestCreateCommitFromDiffs_DeleteApplies_PathGone(t *testing.T) {
	repo := initTestRepo(t)
	cs := NewCommitStore(repo)
	ds := NewDiffStore(repo)
	ctx := context.Background()

	v1 := writeAndCommit(t, repo, "MGIT-1", map[string]string{"keep.txt": "k\n"})
	v2 := writeAndCommit(t, repo, "MGIT-1", map[string]string{"drop.txt": "d\n"})

	// v2 -> v1 removes drop.txt.
	diffs, err := ds.DiffCommits(ctx, v2, v1)
	require.NoError(t, err)

	c := makeTestModelCommit(t, "MGIT-1")
	c.FileDiffs = nil
	hash, err := cs.CreateCommitFromDiffs(ctx, c, diffs)
	require.NoError(t, err)

	_, err = cs.GetFileFromCommit(ctx, hash, "drop.txt")
	assert.Error(t, err, "deleted path must not exist in the new tree")
	got, err := cs.GetFileFromCommit(ctx, hash, "keep.txt")
	require.NoError(t, err)
	assert.Equal(t, "k\n", string(got))
}

// TestCreateCommitFromDiffs_OldHashMismatch_Conflict: applying a diff whose
// OldHash no longer matches the parent tree (someone changed the file since)
// must fail with ErrContentConflict, never silently clobber. Refs: MGIT-54
func TestCreateCommitFromDiffs_OldHashMismatch_Conflict(t *testing.T) {
	repo := initTestRepo(t)
	cs := NewCommitStore(repo)
	ds := NewDiffStore(repo)
	ctx := context.Background()

	v1 := writeAndCommit(t, repo, "MGIT-1", map[string]string{"file.txt": "one\n"})
	v2 := writeAndCommit(t, repo, "MGIT-1", map[string]string{"file.txt": "two\n"})
	// HEAD moves past v2: the same path changes again.
	_ = writeAndCommit(t, repo, "MGIT-1", map[string]string{"file.txt": "three\n"})

	// Inverse of v1->v2 expects the path to still be at v2's blob — it is not.
	diffs, err := ds.DiffCommits(ctx, v2, v1)
	require.NoError(t, err)

	c := makeTestModelCommit(t, "MGIT-1")
	c.FileDiffs = nil
	_, err = cs.CreateCommitFromDiffs(ctx, c, diffs)
	require.Error(t, err)
	assert.ErrorIs(t, err, model.ErrContentConflict)
	assert.Contains(t, err.Error(), "file.txt", "conflict error must name the path")
}

// TestCreateCommitFromDiffs_AddConflicts is the table over Added-diff edge
// cases: adding over an existing different blob conflicts; re-adding an
// identical blob is an idempotent no-op. Refs: MGIT-54
func TestCreateCommitFromDiffs_AddConflicts(t *testing.T) {
	tests := []struct {
		name    string
		mutate  func(t *testing.T, repo *Repository) // move HEAD before applying
		wantErr bool
	}{
		{
			name: "add_over_existing_different_content_conflicts",
			mutate: func(t *testing.T, repo *Repository) {
				writeAndCommit(t, repo, "MGIT-1", map[string]string{"new.txt": "different\n"})
			},
			wantErr: true,
		},
		{
			// new.txt is already at the tip with the identical blob.
			name:    "add_identical_blob_is_noop_ok",
			mutate:  func(t *testing.T, repo *Repository) {},
			wantErr: false,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			repo := initTestRepo(t)
			cs := NewCommitStore(repo)
			ds := NewDiffStore(repo)
			ctx := context.Background()

			base := writeAndCommit(t, repo, "MGIT-1", map[string]string{"a.txt": "base\n"})
			added := writeAndCommit(t, repo, "MGIT-1", map[string]string{"new.txt": "n\n"})

			// The base->added diff adds new.txt.
			addDiff, err := ds.DiffCommits(ctx, base, added)
			require.NoError(t, err)

			tt.mutate(t, repo)
			c := makeTestModelCommit(t, "MGIT-1")
			c.FileDiffs = nil
			_, err = cs.CreateCommitFromDiffs(ctx, c, addDiff)
			if tt.wantErr {
				require.Error(t, err)
				assert.ErrorIs(t, err, model.ErrContentConflict)
			} else {
				assert.NoError(t, err)
			}
		})
	}
}

// TestCreateCommitFromDiffs_StagingUntouched: the primitive must neither
// consume nor clear the CLI staging area — staged work-in-progress survives a
// revert/pick commit. Refs: MGIT-54
func TestCreateCommitFromDiffs_StagingUntouched(t *testing.T) {
	repo := initTestRepo(t)
	cs := NewCommitStore(repo)
	ds := NewDiffStore(repo)
	ctx := context.Background()

	v1 := writeAndCommit(t, repo, "MGIT-1", map[string]string{"file.txt": "good\n"})
	v2 := writeAndCommit(t, repo, "MGIT-1", map[string]string{"file.txt": "bad\n"})

	// Stage unrelated work-in-progress.
	require.NoError(t, os.WriteFile(filepath.Join(repo.Root(), "wip.txt"), []byte("wip\n"), 0o600))
	require.NoError(t, NewWorktreeStore(repo).Add(ctx, "wip.txt"))

	diffs, err := ds.DiffCommits(ctx, v2, v1)
	require.NoError(t, err)

	c := makeTestModelCommit(t, "MGIT-1")
	c.FileDiffs = nil
	hash, err := cs.CreateCommitFromDiffs(ctx, c, diffs)
	require.NoError(t, err)

	// The new tree must NOT contain the staged file...
	_, err = cs.GetFileFromCommit(ctx, hash, "wip.txt")
	assert.Error(t, err, "staged WIP must not leak into a diff-built commit")
	// ...and staging must still hold it.
	staged, err := repo.stagedPaths()
	require.NoError(t, err)
	assert.Contains(t, staged, "wip.txt", "staging must survive the diff-built commit")
}

// TestCreateCommitFromDiffs_EmptyDiffs_Error: an empty diff set is a caller
// bug; refuse rather than mint a no-op commit. Refs: MGIT-54
func TestCreateCommitFromDiffs_EmptyDiffs_Error(t *testing.T) {
	repo := initTestRepo(t)
	cs := NewCommitStore(repo)
	ctx := context.Background()
	writeAndCommit(t, repo, "MGIT-1", map[string]string{"a.txt": "a\n"})

	c := makeTestModelCommit(t, "MGIT-1")
	c.FileDiffs = nil
	_, err := cs.CreateCommitFromDiffs(ctx, c, nil)
	assert.Error(t, err)
}

// TestDiffCommits_ExecutableMode_Preserved: changeToFileDiff must carry the
// file mode so a restore/pick rebuilt from diffs keeps the executable bit.
// Refs: MGIT-54
func TestDiffCommits_ExecutableMode_Preserved(t *testing.T) {
	repo := initTestRepo(t)
	ds := NewDiffStore(repo)
	ctx := context.Background()

	base := writeAndCommit(t, repo, "MGIT-1", map[string]string{"a.txt": "a\n"})
	p := filepath.Join(repo.Root(), "run.sh")
	require.NoError(t, os.WriteFile(p, []byte("#!/bin/sh\n"), 0o700)) //nolint:gosec // executable fixture is the point
	withExec := commitAll(t, repo, "MGIT-1")

	diffs, err := ds.DiffCommits(ctx, base, withExec)
	require.NoError(t, err)
	var found bool
	for _, d := range diffs {
		if d.Path == "run.sh" {
			found = true
			assert.Equal(t, model.FileModeExecutable, d.Mode, "diff must record the executable mode")
		}
	}
	require.True(t, found, "run.sh diff present")
}
