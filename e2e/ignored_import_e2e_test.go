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

// Exercise the real CLI's explicit foundation capture in both exclusion orders.
// The base drops ignored untracked paths without deleting the user's files or
// mutating their Git repository. Refs: MGIT-290, ADR-008 §3, FR-1.3b
func TestE2E_Resync_IgnoredUntrackedCaptureOrders(t *testing.T) {
	bin := buildMgitBinary(t)
	for _, rule := range []string{"info_exclude", "nested_gitignore"} {
		for _, captureFirst := range []bool{false, true} {
			name := rule + "/exclude_first"
			if captureFirst {
				name = rule + "/capture_first"
			}
			t.Run(name, func(t *testing.T) {
				root := t.TempDir()
				gitCmd(t, root, "init")
				writeIgnoredFixture(t, root, ".gitignore", ".mgit/\n.mtix/\n")
				writeIgnoredFixture(t, root, "metadata/keep.txt", "tracked\n")
				gitCmd(t, root, "add", "-A")
				gitCmd(t, root, "commit", "-m", "seed")
				mustMgit(t, bin, root, "init")
				writeIgnoredFixture(t, root, "metadata/private.txt", "untracked\n")
				if captureFirst {
					mustMgit(t, bin, root, "work", filepath.Join(t.TempDir(), "first"),
						"--task-id", "MGIT-290.FIRST", "--include-uncommitted")
				}
				ignorePath, patterns := ".git/info/exclude", "metadata/*\n"
				if rule == "nested_gitignore" {
					ignorePath, patterns = "metadata/.gitignore", "*\n"
				}
				writeIgnoredFixture(t, root, ignorePath, patterns)
				beforeGit := snapshotProjectGit(t, root)
				wt := filepath.Join(t.TempDir(), "second")
				mustMgit(t, bin, root, "work", wt, "--task-id", "MGIT-290.SECOND", "--include-uncommitted")
				assert.NoFileExists(t, filepath.Join(wt, "metadata", "private.txt"))
				assert.Equal(t, "tracked\n", readIgnoredFixture(t, filepath.Join(wt, "metadata", "keep.txt")))
				assert.Equal(t, "untracked\n", readIgnoredFixture(t, filepath.Join(root, "metadata", "private.txt")))
				assert.Equal(t, beforeGit, snapshotProjectGit(t, root))
				assertIgnoredImportBase(t, root)
				out, err := runMgit(t, bin, root, "verify")
				require.NoError(t, err, out)
				out, err = runMgit(t, bin, wt, "verify", "--task-id", "MGIT-290.SECOND")
				require.NoError(t, err, out)
			})
		}
	}
}

func assertIgnoredImportBase(t *testing.T, root string) {
	t.Helper()
	repo, err := gitstore.Open(root, time.Now)
	require.NoError(t, err)
	defer repo.Close() //nolint:errcheck // test fixture
	head, err := repo.Head()
	require.NoError(t, err)
	cs := gitstore.NewCommitStore(repo)
	_, err = cs.GetFileFromCommit(context.Background(), head, "metadata/private.txt")
	assert.Error(t, err, "the shared base excludes ignored untracked content")
}
