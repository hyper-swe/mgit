// Package e2e — a directory given to `mgit add`, in a linked worktree, on the
// real binary (MGIT-276). The reported shape: `mgit add <dir>` stored the
// directory as one staged path, every later commit failed "read working file
// …: is a directory", and nothing could unstage it. Refs: MGIT-276
package e2e

import (
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

func TestE2E_AddADirectory_CommitsTheFilesUnderIt(t *testing.T) {
	bin, wt := stagingWorktree(t)

	mustMgit(t, bin, wt, "add", "other.go")
	mustMgit(t, bin, wt, "add", "internal/transport")
	staging, err := os.ReadFile(filepath.Join(wt, ".mgit", "staging.json")) //nolint:gosec // test-owned path
	require.NoError(t, err)
	assert.NotContains(t, string(staging), `"internal/transport"`, "staging must never hold a directory")

	mustMgit(t, bin, wt, "commit", "-m", "work")
	show := mustMgit(t, bin, wt, "show", "HEAD", "--stat")
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
	assert.NotContains(t, out, "is a directory")

	mustMgit(t, bin, wt, "restore", "--staged", "internal/transport")
	status := mustMgit(t, bin, wt, "status")
	assert.Contains(t, status, "other.go", "unstaging one entry leaves the others staged")
	mustMgit(t, bin, wt, "add", "internal/transport")
	mustMgit(t, bin, wt, "commit", "-m", "work")
	show := mustMgit(t, bin, wt, "show", "HEAD", "--stat")
	assert.Contains(t, show, "internal/transport/t.go")
	assert.Contains(t, show, "other.go")
}

func TestE2E_RestoreStaged_UnstagesExactlyTheNamedPath(t *testing.T) {
	bin, wt := stagingWorktree(t)
	mustMgit(t, bin, wt, "add", "other.go")
	mustMgit(t, bin, wt, "add", "internal/transport")

	mustMgit(t, bin, wt, "restore", "--staged", "other.go")
	mustMgit(t, bin, wt, "commit", "-m", "transport only")
	show := mustMgit(t, bin, wt, "show", "HEAD", "--stat")
	assert.Contains(t, show, "internal/transport/t.go")
	assert.NotContains(t, show, "other.go")
}
