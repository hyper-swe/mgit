package git

import (
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/filemode"
	"github.com/hyper-swe/mgit/internal/model"
	"github.com/stretchr/testify/require"
)

func TestRestorePlanCountsStagedPathsMatchingHead(t *testing.T) {
	repo := initTestRepo(t)
	base := createCommitWithFile(t, repo, "doc", "before", "MGIT-9.2")
	createCommitWithFile(t, repo, "doc", "after", "MGIT-9.2")
	before, local, err := repo.ComputeRestoreDiffs(base)
	require.NoError(t, err)
	require.Len(t, before, 1)
	require.Equal(t, "doc", before[0].Path)
	require.Equal(t, model.DiffModified, before[0].Operation)
	require.Empty(t, local)
	require.NoError(t, NewWorktreeStore(repo).Add(t.Context(), "doc"))
	after, local, err := repo.ComputeRestoreDiffs(base)
	require.NoError(t, err)
	require.Equal(t, before, after)
	require.Equal(t, []string{"doc"}, local)
}

func TestGeneratedBlockWithOnlyWhitespaceOutsideIsKept(t *testing.T) {
	repo := initTestRepo(t)
	const contents = "# BEGIN mgit-sandbox\nsettings\n# END mgit-sandbox\n \t\n"
	require.NoError(t, RecordGeneratedPaths(repo.Root(), []string{"settings"}))
	stageOneFile(t, repo, "settings", contents)
	cs := NewCommitStore(repo)
	tip, err := cs.CreateCommit(t.Context(), makeTestModelCommit(t, "MGIT-9.2"))
	require.NoError(t, err)
	got, err := cs.GetFileFromCommit(t.Context(), tip, "settings")
	require.NoError(t, err)
	require.Equal(t, contents, string(got))
}

func TestGeneratedSymlinkKeepsLiteralTarget(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("uses a Unix file name with embedded newlines")
	}
	repo := initTestRepo(t)
	const target = "first\n# BEGIN mgit-sandbox\nlocal\n# END mgit-sandbox\nlast"
	writeFile(t, repo, target, "referent contents")
	require.NoError(t, os.Symlink(target, filepath.Join(repo.Root(), "shortcut")))
	require.NoError(t, RecordGeneratedPaths(repo.Root(), []string{"shortcut"}))
	require.NoError(t, NewWorktreeStore(repo).Add(t.Context(), "shortcut"))
	cs := NewCommitStore(repo)
	tip, err := cs.CreateCommit(t.Context(), makeTestModelCommit(t, "MGIT-9.2"))
	require.NoError(t, err)
	files, err := repo.headFiles()
	require.NoError(t, err)
	require.Equal(t, filemode.Symlink, files["shortcut"].mode)
	got, err := cs.GetFileFromCommit(t.Context(), tip, "shortcut")
	require.NoError(t, err)
	require.Equal(t, target, string(got))
}

func TestDiffCommitRequiresCommitObjectBeforePublishing(t *testing.T) {
	repo, _, base := faultRepoWithCommit(t)
	stageOneFile(t, repo, "pending", "keep staged")
	blob, err := writeBlob(repo.repo.Storer, []byte("selected"))
	require.NoError(t, err)
	diff := []model.FileDiff{{Path: "selected", Operation: model.DiffAdded, NewHash: blob.String()}}
	create := func() (string, error) {
		return NewCommitStore(repo).CreateCommitFromDiffs(t.Context(), makeTestModelCommit(t, "MGIT-9.2"), diff)
	}
	healthy := repo.repo.Storer
	cause := errors.New("commit object unavailable")
	repo.repo.Storer = snapshotWriteFault{Storer: healthy, kind: plumbing.CommitObject, err: cause}
	failed, err := create()
	repo.repo.Storer = healthy
	require.ErrorIs(t, err, cause)
	require.Empty(t, failed)
	head, err := repo.Head()
	require.NoError(t, err)
	require.Equal(t, base, head)
	staged, err := repo.StagedSnapshot()
	require.NoError(t, err)
	require.Equal(t, []string{"pending"}, staged)
	tip, err := create()
	require.NoError(t, err)
	got, err := NewCommitStore(repo).GetFileFromCommit(t.Context(), tip, "selected")
	require.NoError(t, err)
	require.Equal(t, "selected", string(got))
}
