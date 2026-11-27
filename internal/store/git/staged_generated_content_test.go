package git

import (
	"io/fs"
	"os"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestCommitKeepsWorkingContentsWhenRemovingGeneratedBlocks(t *testing.T) {
	repo := initTestRepo(t)
	head, err := repo.Head()
	require.NoError(t, err)
	ctx := t.Context()
	const block = "# BEGIN mgit-sandbox\nlocal settings\n# END mgit-sandbox\n"
	const mixed = "first line\n" + block + "last line\n"
	cases := []struct{ path, working, committed string }{
		{"mixed.txt", mixed, "first line\nlast line\n"},
		{"block-only.txt", block, block},
		{"ordinary.txt", mixed, mixed},
	}
	for _, tc := range cases {
		stageOneFile(t, repo, tc.path, tc.working)
	}
	selected, err := repo.StagedSnapshot()
	require.NoError(t, err)
	require.Len(t, selected, len(cases))
	list := generatedManifestPath(repo.Root())
	require.NoError(t, os.Mkdir(list, 0o700))
	cs := NewCommitStore(repo)
	failed, err := cs.CreateCommit(ctx, makeTestModelCommit(t, "MGIT-9.2"))
	require.ErrorContains(t, err, "generated manifest")
	require.Empty(t, failed)
	current, err := repo.Head()
	require.NoError(t, err)
	require.Equal(t, head, current)
	staged, err := repo.StagedSnapshot()
	require.NoError(t, err)
	require.Equal(t, selected, staged)

	require.NoError(t, os.Remove(list))
	require.NoError(t, RecordGeneratedPaths(repo.Root(), []string{"mixed.txt", "block-only.txt"}))
	tip, err := cs.CreateCommit(ctx, makeTestModelCommit(t, "MGIT-9.2"))
	require.NoError(t, err)
	for _, tc := range cases {
		got, err := cs.GetFileFromCommit(ctx, tip, tc.path)
		require.NoError(t, err)
		require.Equal(t, tc.committed, string(got), tc.path)
		data, err := fs.ReadFile(os.DirFS(repo.Root()), tc.path)
		require.NoError(t, err)
		require.Equal(t, tc.working, string(data), tc.path)
	}
}
