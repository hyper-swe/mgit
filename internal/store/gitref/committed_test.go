package gitref

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	gogit "github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/object"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// CommittedBlobs is the authority for "what the user's git has committed" — the
// only content a read verb's auto-resync may absorb into the mgit base
// (MGIT-123). These tests pin that it reports git's COMMITTED tree and not the
// working tree, and that it never writes to `.git`. Refs: MGIT-123, ADR-008 §3,§6

// gitRepoWithCommit creates a real git repo with one committed file and returns
// the project root.
func gitRepoWithCommit(t *testing.T, name, content string) string {
	t.Helper()
	dir := t.TempDir()
	repo, err := gogit.PlainInit(dir, false)
	require.NoError(t, err)
	wt, err := repo.Worktree()
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(filepath.Join(dir, name), []byte(content), 0o600))
	_, err = wt.Add(name)
	require.NoError(t, err)
	sig := &object.Signature{Name: "dev", Email: "dev@x", When: time.Unix(0, 0).UTC()}
	_, err = wt.Commit("seed", &gogit.CommitOptions{Author: sig, Committer: sig})
	require.NoError(t, err)
	return dir
}

// TestCommittedBlobs_CommittedFile_ReportedWithBlobID verifies committed content
// is reported keyed by project-relative path. Refs: MGIT-123
func TestCommittedBlobs_CommittedFile_ReportedWithBlobID(t *testing.T) {
	dir := gitRepoWithCommit(t, "main.go", "package main\n")

	blobs, err := CommittedBlobs(dir)
	require.NoError(t, err)
	assert.Equal(t, plumbing.ComputeHash(plumbing.BlobObject, []byte("package main\n")).String(),
		blobs["main.go"])
}

// TestCommittedBlobs_UncommittedFile_NotReported is the property the MGIT-123
// fix rests on: working-tree content git has not committed is absent, so the
// resync cannot absorb it. Refs: MGIT-123
func TestCommittedBlobs_UncommittedFile_NotReported(t *testing.T) {
	dir := gitRepoWithCommit(t, "main.go", "package main\n")
	require.NoError(t, os.WriteFile(filepath.Join(dir, "wip.go"), []byte("package wip\n"), 0o600))
	// A modification to a TRACKED file is likewise not committed content.
	require.NoError(t, os.WriteFile(filepath.Join(dir, "main.go"), []byte("package edited\n"), 0o600))

	blobs, err := CommittedBlobs(dir)
	require.NoError(t, err)
	_, untracked := blobs["wip.go"]
	assert.False(t, untracked, "an untracked file is not committed content")
	assert.Equal(t, plumbing.ComputeHash(plumbing.BlobObject, []byte("package main\n")).String(),
		blobs["main.go"], "the COMMITTED blob is reported, not the edited working file")
}

// TestCommittedBlobs_NoGit_ReturnsErrNoGit verifies the degrade signal callers
// key on. Refs: MGIT-123, ADR-008 §6
func TestCommittedBlobs_NoGit_ReturnsErrNoGit(t *testing.T) {
	_, err := CommittedBlobs(t.TempDir())
	require.Error(t, err)
	assert.True(t, errors.Is(err, ErrNoGit))
}

// TestCommittedBlobs_UnbornHead_ReturnsDetachedOrUnborn verifies a repo with no
// commit degrades rather than failing loud. Refs: MGIT-123, ADR-008 §6
func TestCommittedBlobs_UnbornHead_ReturnsDetachedOrUnborn(t *testing.T) {
	dir := t.TempDir()
	_, err := gogit.PlainInit(dir, false)
	require.NoError(t, err)

	_, err = CommittedBlobs(dir)
	require.Error(t, err)
	assert.True(t, errors.Is(err, ErrDetachedOrUnborn))
}

// TestCommittedBlobs_NeverMutatesDotGit keeps the MGIT-14 / ADR-008 §6
// invariant: reading git's committed tree is a pure read. Refs: MGIT-123, MGIT-14
func TestCommittedBlobs_NeverMutatesDotGit(t *testing.T) {
	dir := gitRepoWithCommit(t, "main.go", "package main\n")
	before := dotGitSnapshot(t, dir)

	_, err := CommittedBlobs(dir)
	require.NoError(t, err)

	assert.Equal(t, before, dotGitSnapshot(t, dir), "CommittedBlobs must never write to .git")
}

// dotGitSnapshot maps every file under <dir>/.git to its bytes.
func dotGitSnapshot(t *testing.T, dir string) map[string]string {
	t.Helper()
	root := filepath.Join(dir, ".git")
	snap := make(map[string]string)
	require.NoError(t, filepath.Walk(root, func(p string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if info.IsDir() {
			return nil
		}
		rel, rerr := filepath.Rel(root, p)
		require.NoError(t, rerr)
		data, derr := os.ReadFile(p) //nolint:gosec // test path
		require.NoError(t, derr)
		snap[rel] = string(data)
		return nil
	}))
	return snap
}
