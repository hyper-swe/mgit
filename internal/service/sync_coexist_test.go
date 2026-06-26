package service

import (
	"context"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	gogit "github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/plumbing/object"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	gitstore "github.com/hyper-swe/mgit/internal/store/git"
	"github.com/hyper-swe/mgit/internal/store/index"
	"github.com/hyper-swe/mgit/internal/store/lock"
)

// gitSnapshot maps every file under <dir>/.git to its bytes — comparing two
// snapshots proves mgit never mutated the project's git repository.
func gitSnapshot(t *testing.T, dir string) map[string]string {
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

// envOverRealGit builds an mgit testEnv whose project root is ALSO a real git
// repo (so the production gitref reader resolves a true local HEAD), exercising
// read-only coexistence end-to-end. Returns the env and the git project dir.
func envOverRealGit(t *testing.T) (*testEnv, string) {
	t.Helper()
	dir := t.TempDir()
	repo, err := gogit.PlainInit(dir, false)
	require.NoError(t, err)
	wt, err := repo.Worktree()
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(filepath.Join(dir, "main.go"), []byte("package main\n"), 0o600))
	_, err = wt.Add("main.go")
	require.NoError(t, err)
	sig := &object.Signature{Name: "dev", Email: "dev@x"}
	_, err = wt.Commit("history", &gogit.CommitOptions{Author: sig, Committer: sig})
	require.NoError(t, err)

	clock := fixedClock()
	mrepo, err := gitstore.Init(dir, clock)
	require.NoError(t, err)
	t.Cleanup(func() { _ = mrepo.Close() })
	idx, err := index.New(filepath.Join(dir, ".mgit", "index.db"), clock)
	require.NoError(t, err)
	t.Cleanup(func() { _ = idx.Close() })
	cs := gitstore.NewCommitStore(mrepo)
	return &testEnv{
		repo: mrepo, cs: cs, bs: gitstore.NewBranchStore(mrepo),
		wt: gitstore.NewWorktreeStore(mrepo), idx: idx,
		commit: NewCommitService(mrepo, cs, idx),
		branch: NewBranchService(mrepo, gitstore.NewBranchStore(mrepo), idx),
	}, dir
}

// TestEnsureSynced_ReadOnlyGit_NeverMutatesDotGit verifies the production
// gitref path (real `.git`) keeps the existing MGIT-14 invariant: a full resync
// reads `.git` to learn git's truth but never writes it. Refs: MGIT-35, ADR-008 §6
func TestEnsureSynced_ReadOnlyGit_NeverMutatesDotGit(t *testing.T) {
	env, dir := envOverRealGit(t)
	ctx := context.Background()

	// A local edit that will trigger a real resync.
	require.NoError(t, os.WriteFile(filepath.Join(dir, "feature.go"), []byte("package feature\n"), 0o600))

	before := gitSnapshot(t, dir)
	svc := NewSyncService(env.repo, env.wt, env.cs, "", fixedClock()) // production gitref reader
	require.NoError(t, svc.EnsureSynced(ctx))
	after := gitSnapshot(t, dir)

	assert.Equal(t, before, after, "EnsureSynced must never mutate the project .git")

	// And the resync actually captured the local file into the base.
	head, err := env.repo.Head()
	require.NoError(t, err)
	got, err := env.cs.GetFileFromCommit(ctx, head, "feature.go")
	require.NoError(t, err)
	assert.Equal(t, "package feature\n", string(got))
}

// TestEnsureSynced_GitFilePointer_Defensive verifies the defensive read of a
// `.git`-as-FILE gitdir pointer (linked git worktree / submodule) drives a
// successful resync rather than crashing. Refs: MGIT-35, ADR-008 §6
func TestEnsureSynced_GitFilePointer_Defensive(t *testing.T) {
	// Real git repo whose .git dir the pointer will reference.
	realDir := t.TempDir()
	rrepo, err := gogit.PlainInit(realDir, false)
	require.NoError(t, err)
	rwt, err := rrepo.Worktree()
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(filepath.Join(realDir, "x.go"), []byte("package x\n"), 0o600))
	_, err = rwt.Add("x.go")
	require.NoError(t, err)
	sig := &object.Signature{Name: "d", Email: "d@x"}
	_, err = rwt.Commit("h", &gogit.CommitOptions{Author: sig, Committer: sig})
	require.NoError(t, err)

	// mgit project dir whose `.git` is a FILE pointing at the real git dir.
	proj := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(proj, ".git"),
		[]byte("gitdir: "+filepath.Join(realDir, ".git")+"\n"), 0o600))
	clock := fixedClock()
	mrepo, err := gitstore.Init(proj, clock)
	require.NoError(t, err)
	t.Cleanup(func() { _ = mrepo.Close() })
	cs := gitstore.NewCommitStore(mrepo)
	ws := gitstore.NewWorktreeStore(mrepo)

	require.NoError(t, os.WriteFile(filepath.Join(proj, "local.go"), []byte("package local\n"), 0o600))
	svc := NewSyncService(mrepo, ws, cs, "", clock)
	require.NoError(t, svc.EnsureSynced(context.Background()), "gitdir-file pointer must resolve, not crash")
}

// TestEnsureSynced_Concurrent_NoCorruption mirrors the real concurrency model:
// distinct processes each open their OWN *Repository over the SAME shared .mgit
// dir and serialize on the store file lock. Each goroutine opens a fresh repo,
// acquires the lock, runs the gate, releases — proving the lock-guarded resync
// converges to one consistent base with no torn state. Run with -race.
// Refs: MGIT-35, ADR-008 §6
func TestEnsureSynced_Concurrent_NoCorruption(t *testing.T) {
	_, dir := envOverRealGit(t)
	require.NoError(t, os.WriteFile(filepath.Join(dir, "c.go"), []byte("package c\n"), 0o600))
	mgitDir := filepath.Join(dir, ".mgit")

	const n = 6
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			fl, lerr := lock.Acquire(mgitDir, lock.DefaultTimeout)
			if lerr != nil {
				return
			}
			defer func() { _ = fl.Release() }()
			clock := func() time.Time { return time.Now().UTC() }
			repo, oerr := gitstore.Open(dir, clock)
			if oerr != nil {
				return
			}
			defer func() { _ = repo.Close() }()
			svc := NewSyncService(repo, gitstore.NewWorktreeStore(repo),
				gitstore.NewCommitStore(repo), "", clock)
			_ = svc.EnsureSynced(context.Background())
		}()
	}
	wg.Wait()

	// The base must still resolve and contain the file — no torn/corrupt base.
	clock := fixedClock()
	repo, err := gitstore.Open(dir, clock)
	require.NoError(t, err)
	defer func() { _ = repo.Close() }()
	cs := gitstore.NewCommitStore(repo)
	head, err := repo.Head()
	require.NoError(t, err)
	got, err := cs.GetFileFromCommit(context.Background(), head, "c.go")
	require.NoError(t, err)
	assert.Equal(t, "package c\n", string(got))
}
