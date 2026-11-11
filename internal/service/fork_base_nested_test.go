package service

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

// A project whose git history committed another tool's store (a `.mgit`
// directory nested in a folder) must still start a task from its committed
// tree: the nested store's bytes are not this project's content, so they are
// left out of the base, and everything committed beside them stays in.

// commitProject writes files (path -> content, relative to the project root)
// to disk, then commits exactly those paths with git. The project root already
// holds the test environment's own `.mgit`.
func commitProject(t *testing.T, env *testEnv, files map[string]string) {
	t.Helper()
	root := env.repo.Root()
	repo, err := gogit.PlainInit(root, false)
	require.NoError(t, err)
	wt, err := repo.Worktree()
	require.NoError(t, err)
	for rel, content := range files {
		abs := filepath.Join(root, filepath.FromSlash(rel))
		require.NoError(t, os.MkdirAll(filepath.Dir(abs), 0o750))
		require.NoError(t, os.WriteFile(abs, []byte(content), 0o600))
		_, err = wt.Add(rel)
		require.NoError(t, err)
	}
	sig := &object.Signature{Name: "dev", Email: "dev@x", When: time.Unix(0, 0).UTC()}
	_, err = wt.Commit("seed", &gogit.CommitOptions{Author: sig, Committer: sig})
	require.NoError(t, err)
}

// forkFromGit builds a task base the way production does, from the project's
// real git history.
func forkFromGit(t *testing.T, env *testEnv) (string, []string) {
	t.Helper()
	svc := NewSyncService(env.repo, env.wt, env.cs, "", fixedClock())
	base, uncommitted, _, ok, err := svc.CommittedForkBase(context.Background())
	require.NoError(t, err, "the committed nested store must not refuse the base")
	require.True(t, ok)
	return base, uncommitted
}

func hasFile(t *testing.T, env *testEnv, base, path string) bool {
	t.Helper()
	_, err := env.cs.GetFileFromCommit(context.Background(), base, path)
	return err == nil
}

// A nested store one folder below the root: the base is built, the store's
// files are absent from it, and the committed file beside it is present and is
// not reported as missing from the checkout.
func TestCommittedForkBase_NestedStoreOneDeep_IsLeftOut(t *testing.T) {
	env := setupTestEnv(t)
	commitProject(t, env, map[string]string{
		"main.go":            "package main\n",
		"archive/keep.go":    "package archive\n",
		"archive/.mgit/obj1": "store bytes\n",
		"archive/.mgit/obj2": "more store bytes\n",
	})

	base, uncommitted := forkFromGit(t, env)

	assert.True(t, hasFile(t, env, base, "main.go"))
	assert.True(t, hasFile(t, env, base, "archive/keep.go"), "a committed sibling of the store stays in the base")
	assert.False(t, hasFile(t, env, base, "archive/.mgit/obj1"))
	assert.False(t, hasFile(t, env, base, "archive/.mgit/obj2"))
	assert.NotContains(t, uncommitted, "archive/keep.go", "a committed sibling must not read as missing")
	assert.NotContains(t, uncommitted, "archive/.mgit/obj1", "the skipped store is not a user deletion")
}

// The same at three folders deep.
func TestCommittedForkBase_NestedStoreThreeDeep_IsLeftOut(t *testing.T) {
	env := setupTestEnv(t)
	commitProject(t, env, map[string]string{
		"main.go":              "package main\n",
		"a/b/c/keep.go":        "package c\n",
		"a/b/c/.mgit/objects":  "store bytes\n",
		"a/b/c/.mgit/sub/more": "deeper store bytes\n",
	})

	base, uncommitted := forkFromGit(t, env)

	assert.True(t, hasFile(t, env, base, "a/b/c/keep.go"))
	assert.False(t, hasFile(t, env, base, "a/b/c/.mgit/objects"))
	assert.False(t, hasFile(t, env, base, "a/b/c/.mgit/sub/more"))
	assert.NotContains(t, uncommitted, "a/b/c/keep.go")
}

// A name that only starts with `.mgit` is ordinary content.
func TestCommittedForkBase_MgitishNameIsOrdinaryContent(t *testing.T) {
	env := setupTestEnv(t)
	commitProject(t, env, map[string]string{
		"x/.mgitish/data": "kept\n",
		"x/.mgit.bak":     "kept too\n",
	})

	base, _ := forkFromGit(t, env)

	assert.True(t, hasFile(t, env, base, "x/.mgitish/data"))
	assert.True(t, hasFile(t, env, base, "x/.mgit.bak"))
}

// The report names each skipped folder once, sorted, however many files the
// store held, and only folders; nothing from inside them.
func TestCommittedForkBase_ReportsEachSkippedFolderOnce(t *testing.T) {
	env := setupTestEnv(t)
	commitProject(t, env, map[string]string{
		"z/.mgit/one":       "x\n",
		"z/.mgit/two/three": "x\n",
		"a/b/c/.mgit/four":  "x\n",
		"a/b/c/.mgit/five":  "x\n",
		"plain/file.go":     "package plain\n",
	})
	svc := NewSyncService(env.repo, env.wt, env.cs, "", fixedClock())

	_, _, skipped, ok, err := svc.CommittedForkBase(context.Background())

	require.NoError(t, err)
	require.True(t, ok)
	assert.Equal(t, []string{"a/b/c/.mgit", "z/.mgit"}, skipped)
}

// A project with no nested store reports nothing skipped.
func TestCommittedForkBase_NoNestedStore_ReportsNothingSkipped(t *testing.T) {
	env := setupTestEnv(t)
	commitProject(t, env, map[string]string{"main.go": "package main\n"})
	svc := NewSyncService(env.repo, env.wt, env.cs, "", fixedClock())

	_, _, skipped, ok, err := svc.CommittedForkBase(context.Background())

	require.NoError(t, err)
	require.True(t, ok)
	assert.Empty(t, skipped)
}

// Uncommitted bytes beside a store are still left out of the base and still
// reported, exactly as they are for any other folder; the store's presence
// does not change what counts as the project's committed content.
func TestCommittedForkBase_SiblingEditAndNewFileBesideStore_AreUncommittedNotImported(t *testing.T) {
	env := setupTestEnv(t)
	commitProject(t, env, map[string]string{
		"main.go":            "package main\n",
		"archive/keep.go":    "package archive\n",
		"archive/.mgit/obj1": "store bytes\n",
	})
	writeProjectFile(t, env, "main.go", "PRIVATE edit\n")
	writeProjectFile(t, env, "new.go", "PRIVATE new\n")

	base, uncommitted := forkFromGit(t, env)

	content, err := env.cs.GetFileFromCommit(context.Background(), base, "main.go")
	require.NoError(t, err)
	assert.Equal(t, "package main\n", string(content), "the committed version, not the edit")
	assert.False(t, hasFile(t, env, base, "new.go"))
	assert.Equal(t, []string{"main.go", "new.go"}, uncommitted)
}

// A store committed in git but absent from this checkout (a fresh clone): the
// folder is walked as an ordinary folder, so the sibling is compared with its
// on-disk copy like any file.
func TestCommittedForkBase_StoreNotOnDisk_SiblingIsComparedAsUsual(t *testing.T) {
	env := setupTestEnv(t)
	commitProject(t, env, map[string]string{
		"archive/keep.go":    "package archive\n",
		"archive/.mgit/obj1": "store bytes\n",
	})
	require.NoError(t, os.RemoveAll(filepath.Join(env.repo.Root(), "archive", ".mgit")))

	base, uncommitted := forkFromGit(t, env)

	assert.True(t, hasFile(t, env, base, "archive/keep.go"))
	assert.False(t, hasFile(t, env, base, "archive/.mgit/obj1"))
	assert.Empty(t, uncommitted, "the sibling is on disk and unchanged")
}

// The base for a project with a committed store is the same tree as the base
// for the same project without the store's files.
func TestCommittedForkBase_TreeEqualsTheProjectWithoutTheStore(t *testing.T) {
	with := setupTestEnv(t)
	commitProject(t, with, map[string]string{
		"main.go":            "package main\n",
		"archive/keep.go":    "package archive\n",
		"archive/.mgit/obj1": "store bytes\n",
	})
	without := setupTestEnv(t)
	commitProject(t, without, map[string]string{
		"main.go":         "package main\n",
		"archive/keep.go": "package archive\n",
	})

	baseWith, _ := forkFromGit(t, with)
	baseWithout, _ := forkFromGit(t, without)

	a, err := with.cs.GetCommit(context.Background(), baseWith)
	require.NoError(t, err)
	b, err := without.cs.GetCommit(context.Background(), baseWithout)
	require.NoError(t, err)
	assert.Equal(t, b.TreeHash, a.TreeHash)
}

// The creation path carries the skipped folders to the caller, and the
// worktree it returns holds none of the store.
func TestWorktreeAdd_ReportsSkippedStoresAndMaterializesNone(t *testing.T) {
	env := setupTestEnv(t)
	commitProject(t, env, map[string]string{
		"archive/keep.go":    "package archive\n",
		"archive/.mgit/obj1": "store bytes\n",
	})
	sync := NewSyncService(env.repo, env.wt, env.cs, "", fixedClock())
	svc := NewWorktreeService(env.idx, env.branch, env.wt, fixedClock()).
		WithSync(sync, env.repo, env.cs)
	path := filepath.Join(t.TempDir(), "wt")

	wt, err := svc.Add(context.Background(), model.WorktreeAddOptions{Path: path, TaskID: "MGIT-1.1"})

	require.NoError(t, err)
	assert.Equal(t, []string{"archive/.mgit"}, wt.SkippedNestedStores)
	assert.FileExists(t, filepath.Join(path, "archive", "keep.go"))
	assert.NoFileExists(t, filepath.Join(path, "archive", ".mgit", "obj1"))
}

// What the narrowing does not cover still fails, as it does today: the root
// store, and a committed file that is itself named `.mgit`.
func TestCommittedForkBase_RootStoreAndTerminalMgitFile_StillRefused(t *testing.T) {
	for name, files := range map[string]map[string]string{
		"root store":    {".mgit/rootfile": "x\n"},
		"terminal file": {"y/.mgit": "a file named like the store\n"},
	} {
		t.Run(name, func(t *testing.T) {
			env := setupTestEnv(t)
			commitProject(t, env, files)
			svc := NewSyncService(env.repo, env.wt, env.cs, "", fixedClock())

			_, _, _, _, err := svc.CommittedForkBase(context.Background())

			require.Error(t, err)
			assert.Contains(t, err.Error(), "excluded directory")
		})
	}
}

// The folder that holds a store is matched as a whole folder: a committed
// folder whose name merely starts with the same letters is another folder, and
// its unchanged files are not reported as uncommitted.
func TestCommittedForkBase_SiblingFolderSharingTheStoreFoldersPrefix_IsNotUncommitted(t *testing.T) {
	env := setupTestEnv(t)
	commitProject(t, env, map[string]string{
		"archive/keep.go":    "package archive\n",
		"archive/.mgit/obj1": "store bytes\n",
		"archive2/x.go":      "package archive2\n",
	})

	base, uncommitted := forkFromGit(t, env)

	assert.True(t, hasFile(t, env, base, "archive2/x.go"))
	assert.Empty(t, uncommitted, "archive2 is an ordinary folder, on disk and unchanged")
}
