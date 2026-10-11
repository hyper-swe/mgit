package e2e

import (
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
)

// Real CLI work on a shared clone reads borrowed maintenance packs without
// modifying either Git store. Refs: MGIT-294, MGIT-14
func TestE2E_Work_SharedCloneAbsoluteAlternates(t *testing.T) {
	bin := buildMgitBinary(t)
	source := t.TempDir()
	gitCmd(t, source, "init")
	writeProjectFile(t, source, ".gitignore", ".mgit/\n")
	writeProjectFile(t, source, "file.txt", "borrowed\n")
	gitCmd(t, source, "add", "-A")
	gitCmd(t, source, "commit", "-m", "seed")
	gitCmd(t, source, "maintenance", "run", "--task=loose-objects")
	gitCmd(t, source, "maintenance", "run", "--task=loose-objects")
	clone := filepath.Join(t.TempDir(), "shared")
	gitCmd(t, source, "clone", "--shared", source, clone)
	beforeSource, beforeClone := snapshotProjectGit(t, source), snapshotProjectGit(t, clone)
	mustMgit(t, bin, clone, "init")
	task := filepath.Join(t.TempDir(), "task")
	mustMgit(t, bin, clone, "work", task, "--task-id", "MGIT-294.CLI")
	assert.Equal(t, "borrowed\n", readFileOrEmpty(t, filepath.Join(task, "file.txt")))
	assert.Equal(t, beforeSource, snapshotProjectGit(t, source))
	assert.Equal(t, beforeClone, snapshotProjectGit(t, clone))
}
