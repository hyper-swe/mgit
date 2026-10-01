// Package e2e — a directory given to `mgit add`, in a linked worktree, on the
// real binary (MGIT-276). The reported shape: `mgit add <dir>` stored the
// directory as one staged path, every later commit failed "read working file
// …: is a directory", and nothing could unstage it. Refs: MGIT-276
package e2e

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// stagingWorktree is a project with one linked worktree bound to a task.
func stagingWorktree(t *testing.T) (bin, wt string) {
	t.Helper()
	bin = buildMgitBinary(t)
	repo := t.TempDir()
	mustMgit(t, bin, repo, "init")
	commitFile(t, bin, repo, "MGIT-276", "seed.txt", "seed\n")
	wt = filepath.Join(t.TempDir(), "wt")
	mustMgit(t, bin, repo, "worktree", "add", "--task", "MGIT-276.E2E", wt)
	for rel, content := range map[string]string{"internal/transport/t.go": "package t\n", "internal/transport/sub/u.go": "package u\n", "other.go": "package o\n"} {
		p := filepath.Join(wt, rel)
		require.NoError(t, os.MkdirAll(filepath.Dir(p), 0o750))
		require.NoError(t, os.WriteFile(p, []byte(content), 0o600))
	}
	return bin, wt
}

// headStat is `mgit show --stat` of the newest commit on the worktree's branch.
func headStat(t *testing.T, bin, wt string) string {
	t.Helper()
	var commits []struct {
		CommitID string `json:"commit_id"`
	}
	require.NoError(t, json.Unmarshal([]byte(mustMgit(t, bin, wt, "log", "--json")), &commits))
	require.NotEmpty(t, commits)
	return mustMgit(t, bin, wt, "show", commits[0].CommitID, "--stat")
}

func TestE2E_AddADirectory_CommitsTheFilesUnderIt(t *testing.T) {
	bin, wt := stagingWorktree(t)

	mustMgit(t, bin, wt, "add", "other.go")
	mustMgit(t, bin, wt, "add", "internal/transport")
	staging, err := os.ReadFile(filepath.Join(wt, ".mgit", "staging.json")) //nolint:gosec // test-owned path
	require.NoError(t, err)
	assert.NotContains(t, string(staging), `"internal/transport"`, "staging must never hold a directory")

	mustMgit(t, bin, wt, "commit", "-m", "work")
	show := headStat(t, bin, wt)
	for _, f := range []string{"internal/transport/t.go", "internal/transport/sub/u.go", "other.go"} {
		assert.Contains(t, show, f)
	}
}

// A staging file left by mgit 0.6.8 still names the directory. Commit refuses
// it by name with the way out, and that way out works.
func TestE2E_StagedDirectoryFromAnOlderMgit_IsRefusedAndRecoverable(t *testing.T) {
	bin, wt := stagingWorktree(t)
	mustMgit(t, bin, wt, "add", "other.go")
	require.NoError(t, os.WriteFile(filepath.Join(wt, ".mgit", "staging.json"),
		[]byte(`{"paths":["internal/transport","other.go"]}`), 0o600))

	out, err := runMgit(t, bin, wt, "commit", "-m", "work")
	require.Error(t, err, out)
	assert.Contains(t, out, "mgit restore --staged internal/transport")
	assert.NotContains(t, out, "read working file", "the old opaque read error is gone")

	mustMgit(t, bin, wt, "restore", "--staged", "internal/transport")
	left, err := os.ReadFile(filepath.Join(wt, ".mgit", "staging.json")) //nolint:gosec // test-owned path
	require.NoError(t, err)
	assert.JSONEq(t, `{"paths":["other.go"]}`, string(left), "unstaging one entry leaves the others staged")
	mustMgit(t, bin, wt, "add", "internal/transport")
	mustMgit(t, bin, wt, "commit", "-m", "work")
	show := headStat(t, bin, wt)
	assert.Contains(t, show, "internal/transport/t.go")
	assert.Contains(t, show, "other.go")
}

func TestE2E_RestoreStaged_UnstagesExactlyTheNamedPath(t *testing.T) {
	bin, wt := stagingWorktree(t)
	mustMgit(t, bin, wt, "add", "other.go")
	mustMgit(t, bin, wt, "add", "internal/transport")

	mustMgit(t, bin, wt, "restore", "--staged", "other.go")
	mustMgit(t, bin, wt, "commit", "-m", "transport only")
	show := headStat(t, bin, wt)
	assert.Contains(t, show, "internal/transport/t.go")
	assert.NotContains(t, show, "other.go")
}

// THE REVIEW'S CASE on the real binary: a tracked file replaced by a
// directory, `add x`, commit; the task's diff and patch still work.
func TestE2E_FileReplacedByDirectory_CommitsAndTheTaskStillDiffs(t *testing.T) {
	bin, wt := stagingWorktree(t)
	require.NoError(t, os.WriteFile(filepath.Join(wt, "x"), []byte("file\n"), 0o600))
	mustMgit(t, bin, wt, "add", "x")
	mustMgit(t, bin, wt, "commit", "-m", "x as a file")
	require.NoError(t, os.Remove(filepath.Join(wt, "x")))
	require.NoError(t, os.MkdirAll(filepath.Join(wt, "x"), 0o750))
	require.NoError(t, os.WriteFile(filepath.Join(wt, "x", "z"), []byte("z\n"), 0o600))

	mustMgit(t, bin, wt, "add", "x")
	mustMgit(t, bin, wt, "commit", "-m", "x as a directory")
	mustMgit(t, bin, wt, "diff", "--task-id", "MGIT-276.E2E")
	patch := mustMgit(t, bin, wt, "squash", "--task-id", "MGIT-276.E2E", "--to-git")
	assert.Contains(t, patch, "x/z")
}
