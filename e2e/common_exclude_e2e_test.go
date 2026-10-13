package e2e

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	gitstore "github.com/hyper-swe/mgit/internal/store/git"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// First capture and later resync honor common exclusions without retargeting
// an existing task or changing native Git bytes. Refs: MGIT-295.
func TestE2E_Work_CommonExclude(t *testing.T) {
	bin := buildMgitBinary(t)
	for _, capture := range []bool{false, true} {
		for _, linked := range []bool{false, true} {
			for _, late := range []bool{false, true} {
				name := "normal/first"
				if linked {
					name = "linked/first"
				}
				if late {
					name += "/later"
				}
				if capture {
					name += "/capture"
				} else {
					name += "/default"
				}
				t.Run(name, func(t *testing.T) {
					root := t.TempDir()
					gitCmd(t, root, "init")
					writeProjectFile(t, root, ".gitignore", ".mgit/\n.mtix/\n!overlay/root-kept.log\n")
					writeProjectFile(t, root, "overlay/tracked.log", "tracked\n")
					gitCmd(t, root, "add", "-A")
					gitCmd(t, root, "commit", "-m", "seed")
					project := root
					if linked {
						project = filepath.Join(t.TempDir(), "linked")
						gitCmd(t, root, "worktree", "add", "--detach", project, "HEAD")
					}
					mustMgit(t, bin, project, "init")
					for _, p := range []string{"overlay/private.log", "overlay/keep.log", "overlay/root-kept.log", "ordinary.txt", ".hyperswe/receipts/run.log"} {
						writeProjectFile(t, project, p, p+"\n")
					}
					first := filepath.Join(t.TempDir(), "first")
					var firstMarker *gitstore.WorktreeMarker
					if late {
						mustMgit(t, bin, project, "work", first, "--task-id", "MGIT-295.FIRST", "--include-uncommitted")
						require.FileExists(t, filepath.Join(first, "overlay", "private.log"))
						var ok bool
						var err error
						firstMarker, ok, err = gitstore.ReadWorktreeMarker(first)
						require.NoError(t, err)
						require.True(t, ok)
						require.NotEmpty(t, firstMarker.ForkBase)
					}
					writeProjectFile(t, root, ".git/info/exclude", "overlay/*\n!overlay/keep.log\n/.hyperswe/receipts/\n")
					gitCmd(t, project, "check-ignore", "overlay/private.log", ".hyperswe/receipts/run.log")
					before := snapshotProjectGit(t, root)
					task := filepath.Join(t.TempDir(), "second")
					args := []string{"work", task, "--task-id", "MGIT-295.SECOND"}
					if capture {
						args = append(args, "--include-uncommitted")
					}
					mustMgit(t, bin, project, args...)
					assert.NoFileExists(t, filepath.Join(task, "overlay", "private.log"))
					assert.NoFileExists(t, filepath.Join(task, ".hyperswe", "receipts", "run.log"))
					assert.FileExists(t, filepath.Join(task, "overlay", "tracked.log"))
					if capture {
						for _, p := range []string{"overlay/keep.log", "overlay/root-kept.log", "ordinary.txt"} {
							assert.FileExists(t, filepath.Join(task, p))
						}
					}
					repo, err := gitstore.Open(project, time.Now)
					require.NoError(t, err)
					defer repo.Close() //nolint:errcheck // fixture
					head, err := repo.Head()
					require.NoError(t, err)
					cs := gitstore.NewCommitStore(repo)
					for _, p := range []string{"overlay/private.log", ".hyperswe/receipts/run.log"} {
						if capture || !late {
							_, err = cs.GetFileFromCommit(context.Background(), head, p)
							assert.Error(t, err)
						}
						assert.FileExists(t, filepath.Join(project, p))
					}
					if late {
						assert.FileExists(t, filepath.Join(first, "overlay", "private.log"))
						assert.FileExists(t, filepath.Join(first, ".hyperswe", "receipts", "run.log"))
						after, ok, err := gitstore.ReadWorktreeMarker(first)
						require.NoError(t, err)
						require.True(t, ok)
						assert.Equal(t, firstMarker, after, "active task base stays pinned")
					}
					assert.Equal(t, before, snapshotProjectGit(t, root))
				})
			}
		}
	}
}

// Checkpoints refuse stale Git pointers without importing unknown exclusions.
// Restoring and repairing the native worktree permits a retry. Refs: MGIT-295.
func TestE2E_Commit_StaleLinkedGitdir(t *testing.T) {
	bin := buildMgitBinary(t)
	root := t.TempDir()
	gitCmd(t, root, "init")
	writeProjectFile(t, root, ".gitignore", ".mgit/\n.mtix/\n")
	writeProjectFile(t, root, "tracked.txt", "seed\n")
	gitCmd(t, root, "add", "-A")
	gitCmd(t, root, "commit", "-m", "seed")
	project := filepath.Join(t.TempDir(), "linked")
	gitCmd(t, root, "worktree", "add", "--detach", project, "HEAD")
	writeProjectFile(t, root, ".git/info/exclude", "private.log\n")
	mustMgit(t, bin, project, "init")
	mustMgit(t, bin, project, "commit", "--allow-empty", "--task-id", "MGIT-295", "-m", "first") // the checkout is already the base
	pointer, err := os.ReadFile(filepath.Join(project, ".git"))                                  //nolint:gosec // read-only native Git fixture pointer
	require.NoError(t, err)
	target := strings.TrimSpace(strings.TrimPrefix(string(pointer), "gitdir:"))
	repo, err := gitstore.Open(project, time.Now)
	require.NoError(t, err)
	defer repo.Close() //nolint:errcheck // fixture
	before, err := repo.Head()
	require.NoError(t, err)
	moved := filepath.Join(t.TempDir(), "moved-git")
	require.NoError(t, os.Rename(filepath.Join(root, ".git"), moved))
	writeProjectFile(t, project, "tracked.txt", "changed\n")
	writeProjectFile(t, project, "private.log", "excluded\n")
	out, err := runMgit(t, bin, project, "commit", "-a", "--task-id", "MGIT-295", "-m", "second")
	require.Error(t, err, "checkpoint must refuse: %s", out)
	assert.Contains(t, out, target)
	assert.Contains(t, out, "git worktree repair")
	assert.Contains(t, out, "recreate the worktree")
	after, err := repo.Head()
	require.NoError(t, err)
	assert.Equal(t, before, after, "refusal creates no checkpoint")
	afterPointer, err := os.ReadFile(filepath.Join(project, ".git")) //nolint:gosec // read-only native Git fixture pointer
	require.NoError(t, err)
	assert.Equal(t, pointer, afterPointer, "refusal does not repair native Git itself")
	require.NoError(t, os.Rename(moved, filepath.Join(root, ".git")))
	gitCmd(t, root, "worktree", "repair", project)
	mustMgit(t, bin, project, "commit", "-a", "--task-id", "MGIT-295", "-m", "second")
	after, err = repo.Head()
	require.NoError(t, err)
	assert.NotEqual(t, before, after)
	cs := gitstore.NewCommitStore(repo)
	_, err = cs.GetFileFromCommit(context.Background(), after, "private.log")
	assert.Error(t, err, "repaired retry honors shared exclusions")
	assert.FileExists(t, filepath.Join(project, "private.log"))
}
