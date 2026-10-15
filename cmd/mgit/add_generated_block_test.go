package main

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	gitstore "github.com/hyper-swe/mgit/internal/store/git"
)

// `mgit work` appends its own generated block to the worktree's CLAUDE.md and
// the block tells the agent that a real directive edit is staged by name. Staging
// by name used to commit the whole file, block included: mgit's per-worktree,
// per-host scaffolding landed in the project's directive. The committed file now
// carries the edit and none of the block, and the working copy keeps the block.
// Refs: MGIT-126, MGIT-80
func TestAddByName_AGeneratedBlockIsNotCommitted(t *testing.T) {
	repo := t.TempDir()
	t.Chdir(repo)
	require.NoError(t, runCLI(t, "init", "--no-agent-files"))
	claude := filepath.Join(repo, "CLAUDE.md")
	require.NoError(t, os.WriteFile(claude, []byte("# Project\n\nrule one\n"), 0o600))
	require.NoError(t, runCLI(t, "commit", "-a", "--task-id", "BASE-1", "-m", "directive"))

	wt := filepath.Join(t.TempDir(), "wt")
	require.NoError(t, runCLI(t, "work", "--task-id", "GEN-1", wt))
	t.Chdir(wt)

	have, err := os.ReadFile(filepath.Join(wt, "CLAUDE.md"))
	require.NoError(t, err)
	require.Contains(t, string(have), "BEGIN mgit-sandbox", "the worktree's CLAUDE.md carries the generated block")
	edited := strings.Replace(string(have), "rule one", "rule one\nrule two (a real edit)", 1)
	require.NoError(t, os.WriteFile(filepath.Join(wt, "CLAUDE.md"), []byte(edited), 0o600))

	require.NoError(t, runCLI(t, "add", "CLAUDE.md"))
	out, errOut, err := runCLICap(t, "commit", "--task-id", "GEN-1", "-m", "edit the directive", "--json")
	require.NoError(t, err, errOut)
	var c struct {
		CommitID string `json:"commit_id"`
	}
	require.NoError(t, json.Unmarshal([]byte(out), &c))

	// The task's commits live in the shared store, so read it from the repo root.
	store, _ := openStore(t, repo)
	committed, err := gitstore.NewCommitStore(store).GetFileFromCommit(context.Background(), c.CommitID, "CLAUDE.md")
	require.NoError(t, err)
	assert.Contains(t, string(committed), "rule two (a real edit)", "the edit is committed")
	assert.NotContains(t, string(committed), "mgit-sandbox", "mgit's generated block is not")
	assert.Equal(t, "# Project\n\nrule one\nrule two (a real edit)\n", string(committed),
		"exactly the user's content: the edit, the rest of the file, and no stray blank line where the block was")

	after, err := os.ReadFile(filepath.Join(wt, "CLAUDE.md"))
	require.NoError(t, err)
	assert.Contains(t, string(after), "BEGIN mgit-sandbox", "the working copy keeps the block")
}
