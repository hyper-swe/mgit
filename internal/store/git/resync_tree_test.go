package git

import (
	"github.com/hyper-swe/mgit/internal/store/gitref"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"testing"
)

// A Git-less project still owns its tracking policy; resync must not throw
// away previously tracked local foundation. Refs: MGIT-290, ADR-008 §6
func TestResyncPaths_NoGit_PreservesPaths(t *testing.T) {
	repo := &Repository{root: t.TempDir()}
	writeFileMk(t, repo.Root(), ".gitignore", "*.local\n")
	paths := []string{"a.go", "keep.local"}
	got, err := repo.resyncPaths(paths)
	require.NoError(t, err)
	assert.Equal(t, paths, got)
}

// An unsupported source cannot be guessed empty, or tracked ignored content
// would disappear from the base. Refs: MGIT-290, ADR-008 §6
func TestResyncPaths_UnreadableGit_FailsLoud(t *testing.T) {
	repo := initTestRepo(t)
	plantUnreadableGit(t, repo.Root())
	writeFileMk(t, repo.Root(), ".gitignore", "*.local\n")
	_, err := repo.resyncPaths([]string{"keep.local"})
	require.ErrorIs(t, err, gitref.ErrUnsupportedGitState)
}
