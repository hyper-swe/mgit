package e2e

import (
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Real CLI work must start over stock Git maintenance packs, including from a
// linked worktree, without touching the user's Git bytes. Refs: FEAT-3.153
func TestE2E_Work_IncrementalMaintenancePacks(t *testing.T) {
	bin := buildMgitBinary(t)
	for _, linked := range []bool{false, true} {
		name := "normal"
		if linked {
			name = "linked"
		}
		t.Run(name, func(t *testing.T) {
			root := t.TempDir()
			gitCmd(t, root, "init")
			writeProjectFile(t, root, ".gitignore", ".mgit/\n")
			writeProjectFile(t, root, "file.txt", "committed\n")
			gitCmd(t, root, "add", "-A")
			gitCmd(t, root, "commit", "-m", "seed")
			project := root
			if linked {
				project = filepath.Join(t.TempDir(), "linked")
				gitCmd(t, root, "worktree", "add", "--detach", project, "HEAD")
			}
			gitCmd(t, root, "maintenance", "run", "--task=loose-objects")
			gitCmd(t, root, "maintenance", "run", "--task=loose-objects")
			packs, err := filepath.Glob(filepath.Join(root, ".git", "objects", "pack", "loose-*.pack"))
			require.NoError(t, err)
			require.NotEmpty(t, packs)
			before := snapshotProjectGit(t, root)
			mustMgit(t, bin, project, "init")
			task := filepath.Join(t.TempDir(), "task")
			mustMgit(t, bin, project, "work", task, "--task-id", "FEAT-3.153.CLI")
			assert.Equal(t, "committed\n", readFileOrEmpty(t, filepath.Join(task, "file.txt")))
			writeProjectFile(t, task, "task.txt", "task work\n")
			mustMgit(t, bin, task, "add", "task.txt")
			mustMgit(t, bin, task, "commit", "-m", "task work")
			patch := mustMgit(t, bin, task, "squash", "--task-id", "FEAT-3.153.CLI", "--to-git")
			assert.Contains(t, patch, "task.txt")
			assert.Equal(t, before, snapshotProjectGit(t, root))
		})
	}
}
