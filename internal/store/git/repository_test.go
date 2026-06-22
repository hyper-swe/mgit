// Package git wraps go-git v5 plumbing API for mgit.
// These tests verify the Repository wrapper per MGIT-2.2.1.
// Refs: FR-1, NFR-4
package git

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	gogit "github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/plumbing/object"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func fixedClock() func() time.Time {
	fixed := time.Date(2026, 4, 7, 12, 0, 0, 0, time.UTC)
	return func() time.Time { return fixed }
}

// initTestRepo creates a Repository OVER a real, pre-existing project `.git`
// and returns it with cleanup. This is the production substrate: mgit always
// runs inside a git-managed project (FR-1, ADR-001). Seeding a real `.git`
// with an EMPTY initial commit (clean worktree) — rather than running in an
// empty temp dir — is what every store/git test now exercises, so the
// read/coexistence paths that the greenfield harness never touched are
// covered. The greenfield-only harness is why MGIT-18 and MGIT-19 shipped.
// Refs: MGIT-14, MGIT-18, MGIT-19, ADR-001
func initTestRepo(t *testing.T) *Repository {
	t.Helper()
	tmpDir := t.TempDir()
	seedProjectGit(t, tmpDir)
	repo, err := Init(tmpDir, fixedClock())
	require.NoError(t, err)
	t.Cleanup(func() { _ = repo.Close() })
	return repo
}

// seedProjectGit initializes a real project `.git` at dir with a single EMPTY
// commit, leaving the worktree clean. mgit must coexist with this `.git` and
// never touch it (TestInit_OverExistingGitRepo_Coexists). The empty commit
// keeps mgit's own staging/commit paths independent of project file state.
func seedProjectGit(t *testing.T, dir string) {
	t.Helper()
	projGit, err := gogit.PlainInit(dir, false)
	require.NoError(t, err)
	wt, err := projGit.Worktree()
	require.NoError(t, err)
	sig := &object.Signature{Name: "dev", Email: "dev@x", When: fixedClock()()}
	_, err = wt.Commit("project: initial", &gogit.CommitOptions{
		Author:            sig,
		Committer:         sig,
		AllowEmptyCommits: true,
	})
	require.NoError(t, err)
}

func TestRepository_Init(t *testing.T) {
	repo := initTestRepo(t)
	assert.DirExists(t, filepath.Join(repo.Root(), ".mgit"))
}

func TestRepository_Init_CreatesDotMgit(t *testing.T) {
	repo := initTestRepo(t)
	mgitDir := filepath.Join(repo.Root(), ".mgit")
	assert.DirExists(t, mgitDir)
	assert.FileExists(t, filepath.Join(mgitDir, "HEAD"))
}

func TestRepository_Init_AlreadyExists(t *testing.T) {
	repo := initTestRepo(t)
	_, err := Init(repo.Root(), fixedClock())
	assert.Error(t, err, "Init on existing repo should fail")
}

func TestRepository_Open(t *testing.T) {
	repo := initTestRepo(t)
	root := repo.Root()
	_ = repo.Close()

	repo2, err := Open(root, fixedClock())
	require.NoError(t, err, "Open must succeed on initialized repo")
	t.Cleanup(func() { _ = repo2.Close() })

	assert.Equal(t, root, repo2.Root())
}

func TestRepository_Open_ValidatesStructure(t *testing.T) {
	tmpDir := t.TempDir()
	_, err := Open(tmpDir, fixedClock())
	assert.Error(t, err, "Open on non-repo directory should fail")
}

func TestRepository_Close(t *testing.T) {
	tmpDir := t.TempDir()
	repo, err := Init(tmpDir, fixedClock())
	require.NoError(t, err)

	err = repo.Close()
	assert.NoError(t, err, "Close must succeed")
}

func TestRepository_ClockInjection(t *testing.T) {
	tmpDir := t.TempDir()
	fixed := time.Date(2026, 6, 15, 10, 30, 0, 0, time.UTC)
	clock := func() time.Time { return fixed }

	repo, err := Init(tmpDir, clock)
	require.NoError(t, err)
	t.Cleanup(func() { _ = repo.Close() })

	assert.Equal(t, fixed, repo.Now(), "Clock must return injected time")
}

func TestRepository_Root(t *testing.T) {
	repo := initTestRepo(t)
	assert.NotEmpty(t, repo.Root())
}

func TestRepository_MgitDir(t *testing.T) {
	repo := initTestRepo(t)
	expected := filepath.Join(repo.Root(), ".mgit")
	assert.Equal(t, expected, repo.MgitDir())
}

func TestRepository_Init_HasInitialCommit(t *testing.T) {
	repo := initTestRepo(t)
	head, err := repo.Head()
	require.NoError(t, err, "initialized repo must have HEAD")
	assert.NotEmpty(t, head, "HEAD must point to a commit")
}

func TestRepository_Init_InvalidPath(t *testing.T) {
	_, err := Init("/nonexistent/path/that/does/not/exist", fixedClock())
	assert.Error(t, err, "Init with invalid path should fail")
}

func TestRepository_Init_NilClock(t *testing.T) {
	tmpDir := t.TempDir()
	_, err := Init(tmpDir, nil)
	assert.Error(t, err, "Init with nil clock should fail")
}

func TestRepository_Open_NilClock(t *testing.T) {
	repo := initTestRepo(t)
	root := repo.Root()
	_ = repo.Close()

	_, err := Open(root, nil)
	assert.Error(t, err, "Open with nil clock should fail")
}

func TestRepository_Init_CorruptedMgitDir(t *testing.T) {
	tmpDir := t.TempDir()
	err := os.WriteFile(filepath.Join(tmpDir, ".mgit"), []byte("not a dir"), 0o600)
	require.NoError(t, err)

	_, err = Init(tmpDir, fixedClock())
	assert.Error(t, err, "Init should fail if .mgit is a file")
}
