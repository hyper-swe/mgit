package git

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestRestorePartialStagingAfterWorkingSetChanges(t *testing.T) {
	repo := initTestRepo(t)
	ws := NewWorktreeStore(repo)
	writeFileMk(t, repo.Root(), "selected", "first")
	writeFileMk(t, repo.Root(), "other", "second")
	require.NoError(t, ws.Add(t.Context(), "selected"))
	saved, err := repo.StagedSnapshot()
	require.NoError(t, err)
	require.Equal(t, []string{"selected"}, saved)

	require.NoError(t, ws.Add(t.Context(), "other"))
	both, err := repo.StagedSnapshot()
	require.NoError(t, err)
	require.ElementsMatch(t, []string{"other", "selected"}, both)
	require.NoError(t, repo.RestoreStaging(saved))
	got, err := repo.StagedSnapshot()
	require.NoError(t, err)
	require.Equal(t, []string{"selected"}, got)
	for _, clear := range []func() error{repo.ClearStaging, func() error { return repo.RestoreStaging(nil) }} {
		require.NoError(t, repo.RestoreStaging(saved))
		require.NoError(t, clear())
		got, err = repo.StagedSnapshot()
		require.NoError(t, err)
		require.Empty(t, got)
	}
	for name, want := range map[string]string{"selected": "first", "other": "second"} {
		data, err := os.ReadFile(filepath.Clean(filepath.Join(repo.Root(), name)))
		require.NoError(t, err)
		require.Equal(t, want, string(data))
	}
}

func TestResyncPrunesIgnoredImportButPreservesHistoryAndDisk(t *testing.T) {
	repo := initTestRepo(t)
	ws, cs := NewWorktreeStore(repo), NewCommitStore(repo)
	ctx := t.Context()
	writeFileMk(t, repo.Root(), "kept", "committed")
	writeFileMk(t, repo.Root(), "old.local", "old content")
	require.NoError(t, ws.Add(ctx, "kept"))
	require.NoError(t, ws.Add(ctx, "old.local"))
	base, err := cs.CreateCommit(ctx, makeTestModelCommit(t, "MGIT-9.2"))
	require.NoError(t, err)
	writeFileMk(t, repo.Root(), ".gitignore", "*.local\n")
	before, err := repo.WorkingTreeFingerprintForResync()
	require.NoError(t, err)
	writeFileMk(t, repo.Root(), "old.local", "local edit")
	after, err := repo.WorkingTreeFingerprintForResync()
	require.NoError(t, err)
	require.Equal(t, before, after, "an ignored import must not drive housekeeping")
	writeFileMk(t, repo.Root(), "kept", "unstaged local edit")
	after, err = repo.WorkingTreeFingerprintForResync()
	require.NoError(t, err)
	require.NotEqual(t, before, after, "an included edit must change the fingerprint")

	equal, err := cs.StagedTreeMatchesHead()
	require.NoError(t, err)
	require.True(t, equal, "nothing was staged after the base commit")
	equal, err = cs.StagedResyncTreeMatchesHead()
	require.NoError(t, err)
	require.False(t, equal, "only the housekeeping tree drops the obsolete import")

	next, err := cs.CreateResyncCommit(ctx, makeTestModelCommit(t, "MGIT-9.2"))
	require.NoError(t, err)
	parents, err := cs.Parents(ctx, next)
	require.NoError(t, err)
	require.Equal(t, []string{base}, parents)
	_, err = cs.GetFileFromCommit(ctx, next, "old.local")
	require.Error(t, err)
	old, err := cs.GetFileFromCommit(ctx, base, "old.local")
	require.NoError(t, err)
	require.Equal(t, "old content", string(old))
	kept, err := cs.GetFileFromCommit(ctx, next, "kept")
	require.NoError(t, err)
	require.Equal(t, "committed", string(kept), "resync must not absorb the unstaged edit")
	disk, err := os.ReadFile(filepath.Join(repo.Root(), "old.local"))
	require.NoError(t, err)
	require.Equal(t, "local edit", string(disk))
	equal, err = cs.StagedResyncTreeMatchesHead()
	require.NoError(t, err)
	require.True(t, equal, "a second housekeeping pass needs no commit")
}
