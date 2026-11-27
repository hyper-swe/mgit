package git

import (
	"io/fs"
	"os"
	"testing"

	"github.com/go-git/go-git/v5/plumbing"
	"github.com/hyper-swe/mgit/internal/model"
	"github.com/stretchr/testify/require"
)

func TestRecoveryCheckpointReportsCommittedFileModes(t *testing.T) {
	repo, faults, _ := faultRepoWithCommit(t)
	target := "notes.txt"
	link, err := writeBlob(repo.repo.Storer, []byte(target))
	require.NoError(t, err)
	executable, err := writeBlob(repo.repo.Storer, []byte("tool contents"))
	require.NoError(t, err)
	tip, err := NewCommitStore(repo).CreateCommitFromDiffs(t.Context(), makeTestModelCommit(t, "MGIT-9.2"), []model.FileDiff{
		{Path: "shortcut", Operation: model.DiffAdded, Mode: model.FileModeSymlink, NewHash: link.String()},
		{Path: "tool", Operation: model.DiffAdded, Mode: model.FileModeExecutable, NewHash: executable.String()},
	})
	require.NoError(t, err)
	// Both working entries are plain files with local contents.
	for _, name := range []string{"shortcut", "tool"} {
		writeFile(t, repo, name, "local contents")
	}
	for _, tc := range []struct {
		path string
		mode model.FileDiffMode
		link string
	}{{"shortcut", model.FileModeSymlink, target}, {"tool", model.FileModeExecutable, ""}} {
		head, entry, err := repo.ProjectionRecoveryCheckpoint(tc.path)
		require.NoError(t, err)
		require.Equal(t, tip, head)
		require.NotNil(t, entry)
		require.Equal(t, tc.path, entry.Path)
		require.True(t, entry.TargetExists)
		require.Equal(t, tc.mode, entry.TargetMode)
		require.Equal(t, tc.link, entry.LinkTarget)
	}

	faults.failHash = link
	head, entry, err := repo.ProjectionRecoveryCheckpoint("shortcut")
	require.ErrorContains(t, err, link.String())
	require.Empty(t, head)
	require.Nil(t, entry)
	faults.failHash = plumbing.ZeroHash
	head, err = repo.Head()
	require.NoError(t, err)
	require.Equal(t, tip, head)
	for _, name := range []string{"shortcut", "tool"} {
		data, err := fs.ReadFile(os.DirFS(repo.Root()), name)
		require.NoError(t, err)
		require.Equal(t, "local contents", string(data))
	}
}
