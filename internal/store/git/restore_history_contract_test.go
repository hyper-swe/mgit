package git

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/hyper-swe/mgit/internal/model"
)

func TestRestorePlanDistinguishesLocalEditsFromHistory(t *testing.T) {
	repo := initTestRepo(t)
	base := writeAndCommit(t, repo, "MGIT-9.2", map[string]string{"edited": "original", "missing": "keep"})
	tip := writeAndCommit(t, repo, "MGIT-9.2", map[string]string{"edited": "committed edit", "added": "new"})
	writeFileMk(t, repo.Root(), "edited", "local edit")
	writeFileMk(t, repo.Root(), "untracked", "private draft")
	missing := filepath.Join(repo.Root(), "missing")
	require.NoError(t, os.Remove(missing))
	plan, dirty, err := repo.ComputeRestoreDiffs(base)
	require.NoError(t, err)
	require.Len(t, plan, 3)
	ops := make(map[string]model.DiffOperation, len(plan))
	for _, d := range plan {
		ops[d.Path] = d.Operation
	}
	require.Equal(t, map[string]model.DiffOperation{
		"added": model.DiffDeleted, "edited": model.DiffModified, "missing": model.DiffAdded,
	}, ops)
	require.ElementsMatch(t, []string{"edited", "missing"}, dirty)

	head, err := repo.currentRef()
	require.NoError(t, err)
	require.Equal(t, tip, head.Hash().String())
	require.NoFileExists(t, missing)
	for name, want := range map[string]string{"edited": "local edit", "added": "new", "untracked": "private draft"} {
		data, err := os.ReadFile(filepath.Clean(filepath.Join(repo.Root(), name)))
		require.NoError(t, err)
		require.Equal(t, want, string(data))
	}
}

func TestJournaledTreeDiffRestoresBytesWithoutConsumingStaging(t *testing.T) {
	repo := initTestRepo(t)
	ctx := t.Context()
	cs, ds := NewCommitStore(repo), NewDiffStore(repo)
	base := writeAndCommit(t, repo, "MGIT-9.2", map[string]string{"doc": "first"})
	tip := writeAndCommit(t, repo, "MGIT-9.2", map[string]string{"doc": "second", "extra": "tip only"})
	tree := func(hash string) string {
		c, err := cs.GetCommit(ctx, hash)
		require.NoError(t, err)
		return c.TreeHash
	}
	inverse, err := ds.DiffTrees(ctx, tree(tip), tree(base))
	require.NoError(t, err)
	require.Len(t, inverse, 2)
	ops := make(map[string]model.DiffOperation, len(inverse))
	for _, diff := range inverse {
		ops[diff.Path] = diff.Operation
	}
	require.Equal(t, map[string]model.DiffOperation{"doc": model.DiffModified, "extra": model.DiffDeleted}, ops)

	writeFileMk(t, repo.Root(), "draft", "keep staged")
	require.NoError(t, NewWorktreeStore(repo).Add(ctx, "draft"))
	entry := ApplyIndexEntry{TaskID: "MGIT-9.2", AgentID: "test-agent"}
	meta := makeTestModelCommit(t, "MGIT-9.2")
	next, err := cs.CreateCommitFromDiffsJournaled(ctx, meta, inverse, entry)
	require.NoError(t, err)
	require.Equal(t, tree(base), tree(next))
	parents, err := cs.Parents(ctx, next)
	require.NoError(t, err)
	require.Equal(t, []string{tip}, parents)
	disk, err := os.ReadFile(filepath.Join(repo.Root(), "doc"))
	require.NoError(t, err)
	require.Equal(t, "first", string(disk))
	require.NoFileExists(t, filepath.Join(repo.Root(), "extra"))
	staged, err := repo.StagedSnapshot()
	require.NoError(t, err)
	require.Equal(t, []string{"draft"}, staged)

	journal, found, err := repo.ReadApplyJournal()
	require.NoError(t, err)
	require.True(t, found)
	require.Equal(t, ApplyJournal{Root: repo.Root(), CommitHash: next, ContentHash: meta.ContentHash,
		Index: entry, Diffs: inverse}, journal)
}
