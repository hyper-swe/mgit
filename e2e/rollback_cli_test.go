// Package e2e — rollback CLI: rolling back by commit hash must resolve the task
// and create the revert in a single App open, not stall on the file lock by
// opening twice. Refs: MGIT-25
package e2e

import (
	"os"
	"path/filepath"
	"regexp"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

var commitIDRe = regexp.MustCompile(`"commit_id":"([a-f0-9]{40})`)

// TestE2E_RollbackByCommitHash_CompletesPromptly drives the real binary:
// `mgit rollback <hash>` resolves the commit's task and creates a revert. If
// the command re-acquired the process lock it would stall for the lock timeout;
// this test would then exceed `go test`'s deadline, so completion IS the
// regression assertion. Refs: MGIT-25
func TestE2E_RollbackByCommitHash_CompletesPromptly(t *testing.T) {
	bin := buildMgitBinary(t)
	repoDir := t.TempDir()
	gitCmd(t, repoDir, "init")
	require.NoError(t, os.WriteFile(filepath.Join(repoDir, "a.go"), []byte("package a\n"), 0o600))
	mustMgit(t, bin, repoDir, "init")
	mustMgit(t, bin, repoDir, "add", "a.go")

	out := mustMgit(t, bin, repoDir, "commit", "--task-id", "MGIT-1", "-m", "seed", "--json")
	m := commitIDRe.FindStringSubmatch(out)
	require.Len(t, m, 2, "could not extract commit id from: %s", out)
	hash := m[1]

	// rollback by positional hash — must complete and create a revert.
	rb := mustMgit(t, bin, repoDir, "rollback", hash[:12])
	assert.Contains(t, rb, "Revert", "rollback by hash must create a revert commit")
	assert.Contains(t, rb, "MGIT-1", "the revert inherits the resolved task")

	// the --to-commit alias takes the same single-open path.
	require.NoError(t, os.WriteFile(filepath.Join(repoDir, "b.go"), []byte("package b\n"), 0o600))
	mustMgit(t, bin, repoDir, "add", "b.go")
	out2 := mustMgit(t, bin, repoDir, "commit", "--task-id", "MGIT-2", "-m", "two", "--json")
	m2 := commitIDRe.FindStringSubmatch(out2)
	require.Len(t, m2, 2)
	rb2 := mustMgit(t, bin, repoDir, "rollback", "--to-commit", m2[1][:12])
	assert.Contains(t, rb2, "Revert")
}
