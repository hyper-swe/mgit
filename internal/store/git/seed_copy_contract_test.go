package git

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestSeedCopyKeepsAbsentSourceOptional(t *testing.T) {
	repo := initTestRepo(t)
	dest := t.TempDir()
	require.NoError(t, repo.copyWorkingFileToDir(dest, "optional/data.txt"))
	require.NoDirExists(t, filepath.Join(dest, "optional"))
}

func TestSeedCopyReplacesOldFileWithRelativeLink(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("requires ordinary symlink creation")
	}
	repo := initTestRepo(t)
	dest := t.TempDir()
	writeFileMk(t, repo.Root(), "assets/version.txt", "v1")
	require.NoError(t, os.Symlink("version.txt", filepath.Join(repo.Root(), "assets", "current")))
	writeFileMk(t, dest, "assets/current", "stale copy")
	require.NoError(t, repo.copyWorkingFileToDir(dest, "assets/current"))
	link, err := os.Readlink(filepath.Join(dest, "assets", "current"))
	require.NoError(t, err)
	require.Equal(t, "version.txt", link)
	require.NoFileExists(t, filepath.Join(dest, "assets", "version.txt"))
}

func TestSeedCopyReportsDestinationConflictAndKeepsExistingContent(t *testing.T) {
	repo := initTestRepo(t)
	const rel = "assets/data.txt"
	writeFileMk(t, repo.Root(), rel, "fresh bytes")
	writeFileMk(t, repo.MgitDir(), "seed-include", rel+"\n")
	for _, tc := range []struct {
		name, marker, want string
	}{
		{"parent is a file", "assets", "mkdir for seed file"},
		{"destination is a directory", rel + "/keep", "write seed file"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dest := t.TempDir()
			writeFileMk(t, dest, tc.marker, "existing bytes")
			err := repo.copySeedIncludes(dest)
			require.ErrorContains(t, err, tc.want+" "+rel)
			got, err := os.ReadFile(filepath.Clean(filepath.Join(dest, tc.marker)))
			require.NoError(t, err)
			require.Equal(t, "existing bytes", string(got))
		})
	}
}
