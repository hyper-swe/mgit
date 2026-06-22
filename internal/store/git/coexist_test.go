package git

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	gogit "github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/plumbing/object"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/hyper-swe/mgit/internal/model"
)

// TestInit_OverExistingGitRepo_Coexists is the executable specification for
// mgit's first-level requirement: mgit runs INSIDE a git-managed project and
// operates OVER AND ABOVE the existing git repo — it must initialize in a
// directory that already has a real `.git`, and it must NEVER create,
// clobber, read, or mutate the project's `.git` (the project's history is
// sacrosanct host state).
//
// It is SKIPPED until the storage re-architecture lands (MGIT-14): today
// Init passes the project root as the go-git worktree, so go-git writes a
// `.git` gitfile at the root and Init fails with "open <root>/.git: is a
// directory" against a real repo. When MGIT-14.2 makes `.mgit/` a
// self-contained (worktree-less) store, remove the t.Skip and this must pass.
//
// This test is the guardrail whose absence let the gap ship: every existing
// test inits mgit in a fresh EMPTY temp dir, so only the greenfield path was
// ever exercised. Refs: MGIT-14, ADR-001 (amendment 2026-06-22)
func TestInit_OverExistingGitRepo_Coexists(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	clk := func() time.Time { return time.Date(2026, 6, 22, 12, 0, 0, 0, time.UTC) }

	// A real git project with pre-existing history.
	projGit, err := gogit.PlainInit(dir, false)
	require.NoError(t, err)
	wt, err := projGit.Worktree()
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(filepath.Join(dir, "main.go"), []byte("package main\n"), 0o600))
	_, err = wt.Add("main.go")
	require.NoError(t, err)
	sig := &object.Signature{Name: "dev", Email: "dev@x", When: clk()}
	historyHead, err := wt.Commit("real project history", &gogit.CommitOptions{Author: sig, Committer: sig})
	require.NoError(t, err)

	gitInfo, err := os.Stat(filepath.Join(dir, ".git"))
	require.NoError(t, err)
	require.True(t, gitInfo.IsDir(), "precondition: the project .git is a real directory")

	// Snapshot the project .git BEFORE mgit touches the directory so we can
	// assert it is byte-for-byte unchanged after a full mgit lifecycle.
	before := snapshotGitDir(t, dir)

	// mgit initializes OVER the existing git project — must succeed.
	repo, err := Init(dir, clk)
	require.NoError(t, err, "mgit must initialize in an existing git project")
	t.Cleanup(func() { _ = repo.Close() })

	// Full mgit lifecycle over the real repo: stage a file, commit it, create a
	// task branch and check it out (materialize), then switch back to main —
	// all via mgit's self-contained .mgit store and plumbing.
	ws := NewWorktreeStore(repo)
	cs := NewCommitStore(repo)
	bs := NewBranchStore(repo)

	require.NoError(t, os.WriteFile(filepath.Join(dir, "feature.go"), []byte("package feature\n"), 0o600))
	require.NoError(t, ws.Add(ctx, "feature.go"))

	c := makeTestModelCommit(t, "MGIT-14.1")
	c.Message = "[MGIT:MGIT-14.1] add feature.go"
	mgitCommit, err := cs.CreateCommit(ctx, c)
	require.NoError(t, err)

	tid, _ := model.ParseTaskID("MGIT-14.2")
	require.NoError(t, bs.CreateBranch(ctx, &model.Branch{Name: "task/MGIT-14.2", HeadCommit: mgitCommit, TaskID: tid}))
	require.NoError(t, ws.Checkout(ctx, "task/MGIT-14.2"))
	require.NoError(t, ws.Checkout(ctx, "main"))

	// The mgit commit's tree contains the staged file (commit landed via plumbing).
	got, err := cs.GetFileFromCommit(ctx, mgitCommit, "feature.go")
	require.NoError(t, err)
	assert.Equal(t, "package feature\n", string(got))

	// The project's git is untouched: still a real .git directory (not turned
	// into a gitfile), and its HEAD/history are byte-for-byte unchanged.
	after, err := os.Stat(filepath.Join(dir, ".git"))
	require.NoError(t, err)
	assert.True(t, after.IsDir(), "mgit must never replace the project's .git directory with a gitfile")

	reopened, err := gogit.PlainOpen(dir)
	require.NoError(t, err, "the project's own git repo must still open normally")
	head, err := reopened.Head()
	require.NoError(t, err)
	assert.Equal(t, historyHead, head.Hash(), "mgit must not move or rewrite the project's HEAD")

	// Hard invariant: the entire .git directory is byte-for-byte identical to
	// its pre-mgit snapshot. mgit must never write into the project's git repo.
	afterSnap := snapshotGitDir(t, dir)
	assert.Equal(t, before, afterSnap, "project .git must be byte-for-byte unchanged after a full mgit lifecycle")
}

// snapshotGitDir returns a map of every file under <dir>/.git keyed by its
// relative path, with the value being the file's exact bytes. Comparing two
// snapshots proves the project's git repository was not mutated by mgit.
func snapshotGitDir(t *testing.T, dir string) map[string]string {
	t.Helper()
	root := filepath.Join(dir, ".git")
	snap := make(map[string]string)
	err := filepath.Walk(root, func(p string, info os.FileInfo, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if info.IsDir() {
			return nil
		}
		rel, err := filepath.Rel(root, p)
		if err != nil {
			return err
		}
		data, err := os.ReadFile(p) //nolint:gosec // test-only path under .git
		if err != nil {
			return err
		}
		snap[rel] = string(data)
		return nil
	})
	require.NoError(t, err)
	return snap
}
