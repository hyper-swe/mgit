package git

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// --- Worktree marker error paths (worktree_marker.go) ---

func TestReadWorktreeMarker_Absent_NotAWorktree(t *testing.T) {
	_, ok, err := ReadWorktreeMarker(t.TempDir())
	require.NoError(t, err)
	assert.False(t, ok, "no marker means a normal repo, not a worktree")
}

func TestReadWorktreeMarker_CorruptJSON_Error(t *testing.T) {
	root := t.TempDir()
	require.NoError(t, os.MkdirAll(filepath.Join(root, mgitDirName), 0o750))
	require.NoError(t, os.WriteFile(filepath.Join(root, mgitDirName, worktreeMarkerName), []byte("{bad"), 0o600))
	_, _, err := ReadWorktreeMarker(root)
	assert.Error(t, err, "a corrupt marker must error")
}

func TestReadWorktreeMarker_MissingFields_Error(t *testing.T) {
	root := t.TempDir()
	require.NoError(t, os.MkdirAll(filepath.Join(root, mgitDirName), 0o750))
	require.NoError(t, os.WriteFile(filepath.Join(root, mgitDirName, worktreeMarkerName), []byte(`{"task":"X"}`), 0o600))
	_, _, err := ReadWorktreeMarker(root)
	assert.Error(t, err, "a marker missing store/branch must error")
}

func TestReadWorktreeMarker_PathIsDir_Error(t *testing.T) {
	root := t.TempDir()
	// Make the marker path a directory so ReadFile fails with a non-NotExist error.
	require.NoError(t, os.MkdirAll(filepath.Join(root, mgitDirName, worktreeMarkerName), 0o750))
	_, _, err := ReadWorktreeMarker(root)
	assert.Error(t, err, "an unreadable marker path must error")
}

func TestWriteWorktreeMarker_RoundTrip(t *testing.T) {
	repo := initTestRepo(t)
	ws := NewWorktreeStore(repo)
	dest := t.TempDir()
	require.NoError(t, ws.WriteWorktreeMarker(dest, "task/MGIT-1", "MGIT-1"))
	m, ok, err := ReadWorktreeMarker(dest)
	require.NoError(t, err)
	require.True(t, ok)
	assert.Equal(t, "task/MGIT-1", m.Branch)
	assert.Equal(t, "MGIT-1", m.Task)
	assert.Equal(t, repo.MgitDir(), m.Store)
}

func TestWriteWorktreeMarker_MkdirError(t *testing.T) {
	repo := initTestRepo(t)
	ws := NewWorktreeStore(repo)
	// A regular file where the worktree's .mgit dir would go blocks MkdirAll.
	dest := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(dest, mgitDirName), []byte("x"), 0o600))
	err := ws.WriteWorktreeMarker(dest, "task/MGIT-1", "MGIT-1")
	assert.Error(t, err, "a blocked .mgit dir must fail the marker write")
}

// --- Repository constructor error paths (repository.go) ---

func TestInit_NilClock_Error(t *testing.T) {
	_, err := Init(t.TempDir(), nil)
	assert.Error(t, err, "a nil clock must be rejected")
}

func TestInit_AlreadyExists_Error(t *testing.T) {
	dir := t.TempDir()
	seedProjectGit(t, dir)
	_, err := Init(dir, fixedClock())
	require.NoError(t, err)
	_, err = Init(dir, fixedClock())
	assert.Error(t, err, "re-init over an existing .mgit must error")
}

func TestInit_MgitIsFile_Error(t *testing.T) {
	dir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(dir, mgitDirName), []byte("x"), 0o600))
	_, err := Init(dir, fixedClock())
	assert.Error(t, err, ".mgit existing as a file must error")
}

func TestOpen_MissingMgit_Error(t *testing.T) {
	_, err := Open(t.TempDir(), fixedClock())
	assert.Error(t, err, "opening a dir without .mgit must error")
}

func TestOpen_MgitIsFile_Error(t *testing.T) {
	dir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(dir, mgitDirName), []byte("x"), 0o600))
	_, err := Open(dir, fixedClock())
	assert.Error(t, err, ".mgit as a file must fail Open")
}

func TestOpen_NilClock_Error(t *testing.T) {
	_, err := Open(t.TempDir(), nil)
	assert.Error(t, err)
}

func TestOpenLinked_NilClock_Error(t *testing.T) {
	_, err := OpenLinked(t.TempDir(), t.TempDir(), "task/X", nil)
	assert.Error(t, err)
}

func TestOpenLinked_MissingParentStore_Error(t *testing.T) {
	_, err := OpenLinked(t.TempDir(), filepath.Join(t.TempDir(), "nope"), "task/X", fixedClock())
	assert.Error(t, err, "a missing parent store must error")
}

// TestCurrentBranch_WorktreeMissingBranch_Error: a linked repo bound to a branch
// that does not exist in the shared store fails to resolve its current ref.
func TestCurrentBranch_WorktreeMissingBranch_Error(t *testing.T) {
	parent := initTestRepo(t)
	linked, err := OpenLinked(t.TempDir(), filepath.Join(parent.Root(), mgitDirName), "task/does-not-exist", fixedClock())
	require.NoError(t, err)
	t.Cleanup(func() { _ = linked.Close() })
	_, err = linked.CurrentBranch()
	assert.Error(t, err, "an unknown bound branch must fail to resolve")
}

// --- Staging error paths (staging.go) ---

func TestLoadStaging_CorruptJSON_Error(t *testing.T) {
	repo := initTestRepo(t)
	require.NoError(t, os.WriteFile(filepath.Join(repo.MgitDir(), stagingFileName), []byte("{not json"), 0o600))
	_, err := repo.stagedPaths()
	assert.Error(t, err, "a corrupt staging file must error")
}

func TestLoadStaging_PathIsDir_Error(t *testing.T) {
	repo := initTestRepo(t)
	require.NoError(t, os.MkdirAll(filepath.Join(repo.MgitDir(), stagingFileName), 0o750))
	_, err := repo.stagedPaths()
	assert.Error(t, err, "an unreadable staging path must error")
}

func TestStagePaths_Idempotent(t *testing.T) {
	repo := initTestRepo(t)
	require.NoError(t, repo.stagePaths([]string{"a.go", "b.go", "a.go"}))
	require.NoError(t, repo.stagePath("a.go")) // duplicate, no-op
	paths, err := repo.stagedPaths()
	require.NoError(t, err)
	assert.ElementsMatch(t, []string{"a.go", "b.go"}, paths)
}

func TestClearStaging_NoFile_NoOp(t *testing.T) {
	repo := initTestRepo(t)
	// No staging file yet — clear must be a no-op, not an error.
	require.NoError(t, repo.clearStaging())
}
