package e2e

import (
	"io/fs"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// The snapshot's visitor and git's transient locks, both shapes of the
// race: a lock that vanished between the listing and the lstat arrives as a
// not-exist error and is skipped; a vanished OBJECT is still an error, since
// that would hide a real change. Refs: MGIT-203
func TestSnapshotVisit_VanishedLockIsSkipped_VanishedObjectIsAnError(t *testing.T) {
	root := filepath.Join(t.TempDir(), ".git")
	visit := snapshotVisit(root, map[string]string{})
	assert.NoError(t, visit(filepath.Join(root, "objects", "maintenance.lock"), nil, fs.ErrNotExist),
		"git's own lock vanishing mid-walk is not evidence about mgit")
	assert.ErrorIs(t, visit(filepath.Join(root, "objects", "aa", "bbbb"), nil, fs.ErrNotExist), fs.ErrNotExist,
		"an object vanishing mid-walk is still reported")
}

// The negative control the ticket asks for: a planted lock must not change
// the snapshot, while a planted object must — the skip removes git's noise
// and nothing else. Refs: MGIT-203
func TestSnapshotProjectGit_PlantedObjectIsSeen_PlantedLockIsNot(t *testing.T) {
	dir := t.TempDir()
	objects := filepath.Join(dir, ".git", "objects", "aa")
	require.NoError(t, os.MkdirAll(objects, 0o750))
	require.NoError(t, os.WriteFile(filepath.Join(dir, ".git", "HEAD"), []byte("ref: refs/heads/main\n"), 0o600))
	before := snapshotProjectGit(t, dir)

	require.NoError(t, os.WriteFile(filepath.Join(dir, ".git", "objects", "maintenance.lock"), []byte("pid\n"), 0o600))
	withLock := snapshotProjectGit(t, dir)
	assert.Equal(t, before, withLock, "a planted lock changes nothing")

	require.NoError(t, os.WriteFile(filepath.Join(objects, "bbbb"), []byte("blob"), 0o600))
	withObject := snapshotProjectGit(t, dir)
	assert.NotEqual(t, before, withObject, "a planted object is a change")
	assert.Contains(t, withObject, filepath.Join("objects", "aa", "bbbb"))
}
