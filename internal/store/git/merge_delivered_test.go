package git

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/hyper-swe/mgit/internal/model"
	"github.com/stretchr/testify/require"
)

func TestFastForwardDelivered_PreflightAndMatchingModes(t *testing.T) {
	for _, kind := range []string{"matching target", "third version", "missing tracked", "wrong executable mode", "wrong symlink mode", "ignored collision", "staged intent", "symlink parent"} {
		t.Run(kind, func(t *testing.T) {
			if runtime.GOOS == "windows" && (kind == "wrong executable mode" || kind == "wrong symlink mode" || kind == "symlink parent") {
				t.Skip("requires Unix executable/symlink modes")
			}
			repo := initTestRepo(t)
			ctx := context.Background()
			base := createCommitWithFile(t, repo, "base.txt", "base\n", "MGIT-317")
			const branch = "task/MGIT-317"
			require.NoError(t, NewBranchStore(repo).CreateBranch(ctx, &model.Branch{Name: branch, HeadCommit: base}))
			createCommitWithFile(t, repo, "base.txt", "tip\n", "MGIT-317")
			createCommitWithFile(t, repo, "new.txt", "new\n", "MGIT-317")
			require.NoError(t, os.MkdirAll(filepath.Join(repo.Root(), "dir"), 0700))
			tip := createCommitWithFile(t, repo, "dir/child.txt", "child\n", "MGIT-317")
			native := t.TempDir()
			require.NoError(t, NewWorktreeStore(repo).WriteWorktreeMarker(native, branch, "MGIT-317"))
			linked, err := OpenLinked(native, repo.MgitDir(), branch, repo.clock)
			require.NoError(t, err)
			defer func() { _ = linked.Close() }()
			write := func(path, data string) {
				t.Helper()
				require.NoError(t, os.WriteFile(filepath.Join(native, path), []byte(data), 0600))
			}
			write("base.txt", "base\n")
			switch kind {
			case "matching target":
				write("base.txt", "tip\n")
				write("new.txt", "new\n")
			case "third version":
				write("base.txt", "host\n")
			case "missing tracked":
				require.NoError(t, os.Remove(filepath.Join(native, "base.txt")))
			case "wrong executable mode":
				write("base.txt", "tip\n")
				require.NoError(t, os.Chmod(filepath.Join(native, "base.txt"), 0700)) //nolint:gosec // G302: intentional executable-mode mismatch fixture
			case "wrong symlink mode":
				require.NoError(t, os.Remove(filepath.Join(native, "base.txt")))
				require.NoError(t, os.Symlink("tip\n", filepath.Join(native, "base.txt")))
			case "ignored collision":
				write(".gitignore", "new.txt\n")
				write("new.txt", "host\n")
			case "symlink parent":
				outside := t.TempDir()
				require.NoError(t, os.WriteFile(filepath.Join(outside, "child.txt"), []byte("child\n"), 0600))
				require.NoError(t, os.Symlink(outside, filepath.Join(native, "dir")))
			case "staged intent":
				require.NoError(t, NewWorktreeStore(linked).Add(ctx, "base.txt"))
			}
			err = NewMergeStore(linked).FastForwardDelivered(ctx, branch, tip)
			if kind == "matching target" {
				require.NoError(t, err)
				head, e := linked.Head()
				require.NoError(t, e)
				require.Equal(t, tip, head)
			} else {
				var conflict *ProjectionConflictError
				require.ErrorAs(t, err, &conflict)
				head, e := linked.Head()
				require.NoError(t, e)
				require.Equal(t, base, head)
				if kind != "ignored collision" {
					_, e = os.Lstat(filepath.Join(native, "new.txt"))
					require.True(t, os.IsNotExist(e), "all conflicts must precede any writes")
				}
			}
		})
	}
}

func TestFastForwardDelivered_TargetDeletion(t *testing.T) {
	for _, alreadyDeleted := range []bool{false, true} {
		t.Run(map[bool]string{false: "old version", true: "already deleted"}[alreadyDeleted], func(t *testing.T) {
			repo := initTestRepo(t)
			ctx := context.Background()
			base := createCommitWithFile(t, repo, "remove.txt", "base\n", "MGIT-317")
			const branch = "task/MGIT-317"
			require.NoError(t, NewBranchStore(repo).CreateBranch(ctx, &model.Branch{Name: branch, HeadCommit: base}))
			require.NoError(t, os.Remove(filepath.Join(repo.Root(), "remove.txt")))
			require.NoError(t, NewWorktreeStore(repo).Add(ctx, "remove.txt"))
			tip, err := NewCommitStore(repo).CreateCommit(ctx, makeTestModelCommit(t, "MGIT-317"))
			require.NoError(t, err)
			native := t.TempDir()
			require.NoError(t, NewWorktreeStore(repo).WriteWorktreeMarker(native, branch, "MGIT-317"))
			if !alreadyDeleted {
				require.NoError(t, os.WriteFile(filepath.Join(native, "remove.txt"), []byte("base\n"), 0600))
			}
			linked, err := OpenLinked(native, repo.MgitDir(), branch, repo.clock)
			require.NoError(t, err)
			defer func() { _ = linked.Close() }()
			require.NoError(t, NewMergeStore(linked).FastForwardDelivered(ctx, branch, tip))
			_, err = os.Lstat(filepath.Join(native, "remove.txt"))
			require.True(t, os.IsNotExist(err))
		})
	}
}
