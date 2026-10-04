// Package e2e — re-adding a worktree for a task that already has a task
// branch (MGIT-275). Drives the real mgit binary through the sequence a
// consumer's recreate path takes: work, commit, lose the worktree, prune,
// work again, commit, then read the task's diff and patch.
// Refs: MGIT-275, MGIT-35, ADR-008 §4
package e2e

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// readdFixture is a git project with mgit initialized, and one task worked,
// committed, and then lost with its worktree directory and pruned.
type readdFixture struct {
	bin, repo, wt, task string
	firstCommit         string // the task's first micro-commit
	forkBase            string // the base that commit was made on
}

func newReaddFixture(t *testing.T) readdFixture {
	t.Helper()
	f := readdFixture{bin: buildMgitBinary(t), repo: t.TempDir(), task: "MGIT-275.E2E"}
	f.wt = filepath.Join(t.TempDir(), "wt")
	gitCmd(t, f.repo, "init")
	require.NoError(t, os.WriteFile(filepath.Join(f.repo, "seed.txt"), []byte("seed\n"), 0o600))
	gitCmd(t, f.repo, "add", "seed.txt")
	gitCmd(t, f.repo, "commit", "-m", "seed")
	mustMgit(t, f.bin, f.repo, "init")

	mustMgit(t, f.bin, f.repo, "work", f.wt, "--task-id", f.task)
	f.forkBase = pinnedForkBase(t, f)
	require.NoError(t, os.WriteFile(filepath.Join(f.wt, "one.txt"), []byte("one\n"), 0o600))
	mustMgit(t, f.bin, f.wt, "add", "one.txt")
	mustMgit(t, f.bin, f.wt, "commit", "-m", "one")
	f.firstCommit = branchHead(t, f, "task/"+f.task)

	require.NoError(t, os.RemoveAll(f.wt), "the worktree directory is lost")
	mustMgit(t, f.bin, f.repo, "worktree", "prune")
	return f
}

// branchHead reads a branch's head commit from `mgit branch --json`.
func branchHead(t *testing.T, f readdFixture, name string) string {
	t.Helper()
	var branches []struct {
		Name       string `json:"name"`
		HeadCommit string `json:"head_commit"`
	}
	require.NoError(t, json.Unmarshal([]byte(mustMgit(t, f.bin, f.repo, "branch", "--json")), &branches))
	for _, b := range branches {
		if b.Name == name {
			return b.HeadCommit
		}
	}
	t.Fatalf("no branch %s", name)
	return ""
}

// pinnedForkBase reads the fork-base the registry pinned for the fixture's task.
func pinnedForkBase(t *testing.T, f readdFixture) string {
	t.Helper()
	var wts []struct {
		TaskID   string `json:"task_id"`
		ForkBase string `json:"fork_base"`
	}
	require.NoError(t, json.Unmarshal([]byte(mustMgit(t, f.bin, f.repo, "worktree", "list", "--json")), &wts))
	for _, w := range wts {
		if w.TaskID == f.task {
			require.NotEmpty(t, w.ForkBase)
			return w.ForkBase
		}
	}
	t.Fatalf("no worktree registered for %s", f.task)
	return ""
}

// commitSecond commits a second file in the re-added worktree.
func (f readdFixture) commitSecond(t *testing.T) {
	t.Helper()
	require.NoError(t, os.WriteFile(filepath.Join(f.wt, "two.txt"), []byte("two\n"), 0o600))
	mustMgit(t, f.bin, f.wt, "add", "two.txt")
	mustMgit(t, f.bin, f.wt, "commit", "-m", "two")
}

// THE REPORTED SEQUENCE. A re-added worktree keeps the task's ORIGINAL
// fork-base — the base its first micro-commit was made on — so the task's
// diff and patch still carry every commit, the ones from before the loss too.
func TestE2E_WorktreeReAdded_KeepsTheTaskForkBase(t *testing.T) {
	f := newReaddFixture(t)

	mustMgit(t, f.bin, f.repo, "work", f.wt, "--task-id", f.task)
	assert.Equal(t, f.forkBase, pinnedForkBase(t, f), "the re-add must pin the original fork-base")
	f.commitSecond(t)

	diff := mustMgit(t, f.bin, f.repo, "diff", "--task-id", f.task)
	assert.Contains(t, diff, "one.txt", "the commit from before the loss is part of the task")
	assert.Contains(t, diff, "two.txt")
	patch := mustMgit(t, f.bin, f.wt, "squash", "--task-id", f.task, "--to-git")
	assert.Contains(t, patch, "+one\n")
	assert.Contains(t, patch, "+two\n")
}

// --base on a re-add may name the task's real fork-base; the re-add then pins
// exactly that.
func TestE2E_WorktreeReAdded_BaseNamingTheForkBase_IsAccepted(t *testing.T) {
	f := newReaddFixture(t)

	mustMgit(t, f.bin, f.repo, "work", f.wt, "--task-id", f.task, "--base", f.forkBase)
	assert.Equal(t, f.forkBase, pinnedForkBase(t, f))
	f.commitSecond(t)
	mustMgit(t, f.bin, f.repo, "diff", "--task-id", f.task)
}

// --base naming any other commit cannot move an existing task branch's
// history, so it is refused, naming the fork-base the branch actually has.
func TestE2E_WorktreeReAdded_BaseNamingAnotherCommit_IsRefused(t *testing.T) {
	f := newReaddFixture(t)

	out, err := runMgit(t, f.bin, f.repo, "work", f.wt, "--task-id", f.task, "--base", f.firstCommit)
	require.Error(t, err, "a --base the task branch did not fork from must be refused: %s", out)
	assert.Contains(t, out, f.forkBase[:8], "the refusal names the fork-base the branch has")
	_, statErr := os.Stat(f.wt)
	assert.True(t, os.IsNotExist(statErr), "a refused re-add creates no worktree")
}
