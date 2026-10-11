package e2e

import (
	"context"
	"path/filepath"
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
