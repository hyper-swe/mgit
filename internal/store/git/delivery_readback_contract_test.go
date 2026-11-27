package git

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestDeliveryReadbackReportsDriftWithoutChangingTheWorktree(t *testing.T) {
	repo := initTestRepo(t)
	ctx := t.Context()
	ms := NewMergeStore(repo)
	base, err := repo.Head()
	require.NoError(t, err)
	tip := createCommitWithFile(t, repo, "doc", "published", "MGIT-9.2")
	writeFileMk(t, repo.Root(), "notes", "untracked")
	require.NoError(t, ms.VerifyDelivered(ctx, tip))
	require.ErrorContains(t, ms.VerifyDelivered(ctx, base), base)

	writeFileMk(t, repo.Root(), "doc", "local version")
	require.ErrorContains(t, ms.VerifyDelivered(ctx, tip), "doc")
	for _, tc := range []struct {
		path    string
		tracked bool
	}{{"doc", true}, {"notes", false}} {
		head, entry, err := repo.ProjectionRecoveryCheckpoint(tc.path)
		require.NoError(t, err)
		require.Equal(t, tip, head)
		require.NotNil(t, entry)
		require.Equal(t, tc.path, entry.Path)
		require.Equal(t, tc.tracked, entry.TargetExists)
		require.Contains(t, entry.Error(), tc.path)
	}
	for name, want := range map[string]string{"doc": "local version", "notes": "untracked"} {
		data, err := os.ReadFile(filepath.Clean(filepath.Join(repo.Root(), name)))
		require.NoError(t, err)
		require.Equal(t, want, string(data))
	}
	writeFileMk(t, repo.Root(), "doc", "published")
	require.NoError(t, NewWorktreeStore(repo).Add(ctx, "notes"))
	require.ErrorContains(t, ms.VerifyDelivered(ctx, tip), "staging")
	staged, err := repo.StagedSnapshot()
	require.NoError(t, err)
	require.Equal(t, []string{"notes"}, staged)
	head, err := repo.Head()
	require.NoError(t, err)
	require.Equal(t, tip, head)
}

func TestGeneratedStoreManifestControlsBulkButNotNamedStaging(t *testing.T) {
	repo := initTestRepo(t)
	ctx := t.Context()
	ws := NewWorktreeStore(repo)
	writeFileMk(t, repo.Root(), "generated.md", "generated")
	writeFileMk(t, repo.Root(), "authored.md", "authored")
	require.NoError(t, WriteGeneratedPathsInStore(repo.MgitDir(), []string{"generated.md"}))
	require.NoError(t, WriteGeneratedPathsInStore(repo.MgitDir(), nil))
	recorded, err := ReadGeneratedPaths(repo.Root())
	require.NoError(t, err)
	require.Equal(t, []string{"generated.md"}, recorded, "empty input leaves the recorded list alone")
	require.NoError(t, ws.Add(ctx, "."))
	staged, err := repo.StagedSnapshot()
	require.NoError(t, err)
	require.Equal(t, []string{"authored.md"}, staged)
	require.NoError(t, ws.Add(ctx, "generated.md"))
	staged, err = repo.StagedSnapshot()
	require.NoError(t, err)
	require.ElementsMatch(t, []string{"authored.md", "generated.md"}, staged)
}
