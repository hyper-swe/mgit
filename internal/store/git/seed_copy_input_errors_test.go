package git

import (
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestMalformedSeedPatternIsNotAnEmptyMatch(t *testing.T) {
	repo := initTestRepo(t)
	dest := t.TempDir()
	writeFileMk(t, repo.Root(), "asset.txt", "source")
	writeFileMk(t, repo.MgitDir(), seedIncludeFileName, "[\n")
	err := repo.copySeedIncludes(dest)
	require.ErrorIs(t, err, filepath.ErrBadPattern)
	require.ErrorContains(t, err, "match seed glob")
	entries, err := os.ReadDir(dest)
	require.NoError(t, err)
	require.Empty(t, entries)
	writeFileMk(t, repo.MgitDir(), seedIncludeFileName, "*.txt\n")
	require.NoError(t, repo.copySeedIncludes(dest))
	data, err := fs.ReadFile(os.DirFS(dest), "asset.txt")
	require.NoError(t, err)
	require.Equal(t, "source", string(data))
}

func TestSeedCopyReadFailurePreservesDestinationFile(t *testing.T) {
	repo := initTestRepo(t)
	dest := t.TempDir()
	require.NoError(t, os.Mkdir(filepath.Join(repo.Root(), "asset"), 0o700))
	writeFileMk(t, dest, "asset", "previous")
	require.ErrorContains(t, repo.copyWorkingFileToDir(dest, "asset"), "read seed file asset")
	data, err := fs.ReadFile(os.DirFS(dest), "asset")
	require.NoError(t, err)
	require.Equal(t, "previous", string(data))
}

func TestSeedLinkDoesNotReplacePopulatedDirectory(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("requires ordinary symlink creation")
	}
	repo := initTestRepo(t)
	dest := t.TempDir()
	writeFileMk(t, repo.Root(), "version.txt", "v1")
	require.NoError(t, os.Symlink("version.txt", filepath.Join(repo.Root(), "current")))
	writeFileMk(t, dest, "current/keep", "previous")
	err := repo.copyWorkingFileToDir(dest, "current")
	require.ErrorContains(t, err, "replace seed file current")
	data, err := fs.ReadFile(os.DirFS(dest), "current/keep")
	require.NoError(t, err)
	require.Equal(t, "previous", string(data))
}
