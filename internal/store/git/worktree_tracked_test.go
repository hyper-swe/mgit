package git

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/hyper-swe/mgit/internal/store/gitref"
)

// plantUnreadableGit gives the project a .git whose committed tree cannot be
// read safely (a shallow clone), so any read of it fails.
func plantUnreadableGit(t *testing.T, root string) {
	t.Helper()
	require.NoError(t, os.MkdirAll(filepath.Join(root, ".git"), 0o750))
	require.NoError(t, os.WriteFile(filepath.Join(root, ".git", "HEAD"), []byte("ref: refs/heads/main\n"), 0o600))
	require.NoError(t, os.WriteFile(filepath.Join(root, ".git", "shallow"), []byte{}, 0o600))
}

// The tracked set is read only when an ignore rule matches a path: a walk no
// rule touches reads neither tree, so an unreadable git costs it nothing.
// Refs: MGIT-269
func TestListWorkingFiles_NoRuleMatches_ReadsNoGit(t *testing.T) {
	repo := initTestRepo(t)
	plantUnreadableGit(t, repo.Root())
	writeFileMk(t, repo.Root(), "a.go", "package a\n")

	paths, err := repo.listWorkingFiles()

	require.NoError(t, err)
	assert.Contains(t, paths, "a.go")
}

// When a rule matches and the project's git cannot say what it tracks, the
// walk fails loud rather than guess: a guess of "nothing" is the defect this
// filter removes, hiding tracked files as if they were ignored. Refs: MGIT-269
func TestListWorkingFiles_RuleMatchesAndGitUnreadable_FailsLoud(t *testing.T) {
	repo := initTestRepo(t)
	plantUnreadableGit(t, repo.Root())
	writeFileMk(t, repo.Root(), ".gitignore", "*.log\n")
	writeFileMk(t, repo.Root(), "app.log", "log\n")

	_, err := repo.listWorkingFiles()

	require.ErrorIs(t, err, gitref.ErrUnsupportedGitState)
}
