package git

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// A linked worktree nested in the project holds its own .mgit, and that
// directory is another store's to change: a concurrent `mgit work` creates and
// removes temporary files in it while a sibling reads the project's ignore
// rules. Reading the rules must never look inside a nested mgit root, because
// one path that vanishes (or cannot be read) mid-read fails the sibling's whole
// worktree add. An unreadable directory is the deterministic form of "that
// path is not ours to read". Refs: MGIT-285, MGIT-120
func TestListWorkingFiles_NestedWorktreeStoreUnreadable_IsNeverRead(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("an unreadable directory is readable to root; the unit test of the pruned reader covers that case")
	}
	root := t.TempDir()
	writeFileMk(t, root, "a.go", "package a\n")
	require.NoError(t, os.MkdirAll(filepath.Join(root, "wt-C-4", ".mgit", "pending"), 0o750))
	writeFileMk(t, root, "wt-C-4/work.go", "package w\n")
	store := filepath.Join(root, "wt-C-4", ".mgit")
	require.NoError(t, os.Chmod(store, 0o000))
	t.Cleanup(func() { _ = os.Chmod(store, 0o750) }) //nolint:gosec // G302: restoring a test directory so t.TempDir can remove it
	r := &Repository{root: root}

	paths, err := r.listWorkingFiles()

	require.NoError(t, err, "reading the ignore rules looked inside a nested worktree's store")
	assert.Equal(t, []string{"a.go"}, paths, "a nested worktree's files are not this project's content")
}
