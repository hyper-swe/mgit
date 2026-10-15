package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func recordedAgents(t *testing.T, taskID string) []string {
	t.Helper()
	out, errOut, err := runCLICap(t, "export", "--task-id", taskID, "--format", "json")
	require.NoError(t, err, errOut)
	var rows []struct {
		AgentID string `json:"agent_id"`
	}
	require.NoError(t, json.Unmarshal([]byte(out), &rows))
	agents := make([]string, 0, len(rows))
	for _, r := range rows {
		agents = append(agents, r.AgentID)
	}
	return agents
}

// A commit made from inside a task-bound worktree records the agent the
// worktree is bound to, the same way it inherits the worktree's task. Before,
// every such commit said "cli", so with parallel agents on one store there was
// no way to attribute a commit. An explicit --agent-id still wins, and outside
// a worktree nothing better is known, so the default stays. Refs: MGIT-117
func TestCommit_InAWorktreeRecordsTheWorktreesAgent(t *testing.T) {
	repo := t.TempDir()
	t.Chdir(repo)
	require.NoError(t, runCLI(t, "init", "--no-agent-files"))
	wt := filepath.Join(t.TempDir(), "wt")
	require.NoError(t, runCLI(t, "work", "--task-id", "AG-1", "--agent-id", "agent-7", wt))

	t.Chdir(wt)
	require.NoError(t, os.WriteFile(filepath.Join(wt, "a.txt"), []byte("one\n"), 0o600))
	require.NoError(t, runCLI(t, "commit", "-a", "-m", "inherits the worktree's agent"))
	require.NoError(t, os.WriteFile(filepath.Join(wt, "a.txt"), []byte("two\n"), 0o600))
	require.NoError(t, runCLI(t, "commit", "-a", "-m", "explicit wins", "--agent-id", "someone-else"))

	t.Chdir(repo)
	assert.Equal(t, []string{"agent-7", "someone-else"}, recordedAgents(t, "AG-1"))

	t.Chdir(wt) // the task's commits are on the worktree's branch
	out, errOut, err := runCLICap(t, "log", "--author", "agent-7", "--json")
	require.NoError(t, err, errOut)
	assert.Contains(t, out, "inherits the worktree's agent", "log --author must return the inherited commit")
	assert.NotContains(t, out, "explicit wins")
}

func TestCommit_OutsideAWorktreeKeepsTheDefaultAgent(t *testing.T) {
	repo := t.TempDir()
	t.Chdir(repo)
	require.NoError(t, runCLI(t, "init", "--no-agent-files"))
	require.NoError(t, os.WriteFile(filepath.Join(repo, "a.txt"), []byte("one\n"), 0o600))
	require.NoError(t, runCLI(t, "commit", "-a", "--task-id", "AG-2", "-m", "no worktree"))
	assert.Equal(t, []string{"cli"}, recordedAgents(t, "AG-2"))
}

// A worktree made without an agent id has nothing to inherit, so the default
// still applies. Refs: MGIT-117
func TestCommit_InAWorktreeWithNoAgentKeepsTheDefault(t *testing.T) {
	repo := t.TempDir()
	t.Chdir(repo)
	require.NoError(t, runCLI(t, "init", "--no-agent-files"))
	wt := filepath.Join(t.TempDir(), "wt")
	require.NoError(t, runCLI(t, "work", "--task-id", "AG-3", wt))
	t.Chdir(wt)
	require.NoError(t, os.WriteFile(filepath.Join(wt, "a.txt"), []byte("one\n"), 0o600))
	require.NoError(t, runCLI(t, "commit", "-a", "-m", "anonymous worktree"))
	t.Chdir(repo)
	assert.Equal(t, []string{"cli"}, recordedAgents(t, "AG-3"))
}
