package git

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/hyper-swe/mgit/internal/store/gitref"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Common info/exclude has lower priority than in-tree .gitignore rules, and
// only ignored Git-untracked inputs leave the resync set. Refs: MGIT-295.
func TestResyncPaths_CommonExclude(t *testing.T) {
	for _, linked := range []bool{false, true} {
		name := "normal"
		if linked {
			name = "linked"
		}
		t.Run(name, func(t *testing.T) {
			root := t.TempDir()
			commonExcludeGit(t, root, "init")
			writeFileMk(t, root, ".gitignore", ".mgit/\n.mtix/\n!overlay/root-kept.log\n")
			writeFileMk(t, root, "overlay/tracked.log", "tracked\n")
			commonExcludeGit(t, root, "add", "-A")
			commonExcludeGit(t, root, "commit", "-m", "seed")
			project := root
			if linked {
				project = filepath.Join(t.TempDir(), "linked")
				commonExcludeGit(t, root, "worktree", "add", "--detach", project, "HEAD")
			}
			exclude := strings.TrimSpace(commonExcludeGit(t, project, "rev-parse", "--git-path", "info/exclude"))
			if !filepath.IsAbs(exclude) {
				exclude = filepath.Join(project, exclude)
			}
			writeFileMk(t, filepath.Dir(exclude), filepath.Base(exclude), "overlay/*\n!overlay/keep.log\n/.hyperswe/receipts/\n")
			commonExcludeGit(t, project, "check-ignore", "overlay/private.log")
			paths := []string{"overlay/private.log", "overlay/tracked.log", "overlay/keep.log", "overlay/root-kept.log", "ordinary.txt", ".hyperswe/receipts/run.log"}
			got, err := (&Repository{root: project}).resyncPaths(paths)
			require.NoError(t, err)
			assert.ElementsMatch(t, []string{"overlay/tracked.log", "overlay/keep.log", "overlay/root-kept.log", "ordinary.txt"}, got)
		})
	}
}

func commonExcludeGit(t *testing.T, root string, args ...string) string {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	full := append([]string{"-c", "user.name=dev", "-c", "user.email=dev@example.com", "-c", "gc.auto=0", "-c", "maintenance.auto=false"}, args...)
	cmd := exec.CommandContext(ctx, "git", full...) //nolint:gosec // fixed native Git fixture
	cmd.Dir = root
	out, err := cmd.CombinedOutput()
	require.NoError(t, err, "git %v: %s", args, out)
	return string(out)
}

// Unresolved exclusions must not silently become an empty matcher. Refs: MGIT-295.
func TestIgnoreMatcher_StaleLinkedGitdir(t *testing.T) {
	root := t.TempDir()
	commonExcludeGit(t, root, "init")
	writeFileMk(t, root, "tracked.txt", "seed\n")
	commonExcludeGit(t, root, "add", "-A")
	commonExcludeGit(t, root, "commit", "-m", "seed")
	project := filepath.Join(t.TempDir(), "linked")
	commonExcludeGit(t, root, "worktree", "add", "--detach", project, "HEAD")
	pointer, err := os.ReadFile(filepath.Join(project, ".git")) //nolint:gosec // read-only native Git fixture pointer
	require.NoError(t, err)
	target := strings.TrimSpace(strings.TrimPrefix(string(pointer), "gitdir:"))
	require.NoError(t, os.Rename(filepath.Join(root, ".git"), filepath.Join(t.TempDir(), "moved-git")))
	matcher, err := (&Repository{root: project}).ignoreMatcher()
	require.ErrorIs(t, err, gitref.ErrUnsupportedGitState)
	assert.Nil(t, matcher, "do not proceed without resolved exclusions")
	assert.Contains(t, err.Error(), target, "name the stale gitdir pointer")
	assert.Contains(t, err.Error(), "git worktree repair")
	assert.Contains(t, err.Error(), "recreate the worktree")
}
