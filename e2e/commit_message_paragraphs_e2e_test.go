package e2e

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	gogit "github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/plumbing/object"
	"github.com/stretchr/testify/require"
)

// Refs: MGIT-296, FR-2.9. Drive the real binary and compare recorded messages.
func TestE2E_Commit_MessageParagraphs(t *testing.T) {
	bin := buildMgitBinary(t)
	for _, tt := range []struct {
		name string
		args []string
		want string
	}{
		{"short", []string{"-m", "a", "-m", "b"}, "a\n\nb"},
		{"mixed", []string{"--message", "a", "-m", "b", "--message=c"}, "a\n\nb\n\nc"},
		{"single", []string{"-m", "a,b\nbody\n"}, "a,b\nbody\n"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			root := t.TempDir()
			mustMgit(t, bin, root, "init")
			args := append([]string{"commit", "--allow-empty", "--task-id", "MGIT-296"}, tt.args...)
			mustMgit(t, bin, root, args...)
			require.Equal(t, "[MGIT:MGIT-296] "+tt.want, paragraphHead(t, root).Message)
		})
	}
	t.Run("message_and_file", func(t *testing.T) { assertParagraphFileRefusal(t, bin) })
	t.Run("git_contract", assertGitParagraphContract)
}

func assertParagraphFileRefusal(t *testing.T, bin string) {
	t.Helper()
	root := t.TempDir()
	mustMgit(t, bin, root, "init")
	before := paragraphHead(t, root)
	file := filepath.Join(t.TempDir(), "message.txt")
	require.NoError(t, os.WriteFile(file, []byte("file message\n"), 0o600))
	require.NoError(t, os.WriteFile(filepath.Join(root, "pending.txt"), []byte("pending"), 0o600))
	for _, flags := range [][]string{{"-m", "a", "-m", "b", "-F", file}, {"-F", file, "--message", "a"}} {
		args := append([]string{"commit", "-a", "--task-id", "MGIT-296"}, flags...)
		out, err := runMgit(t, bin, root, args...)
		require.Error(t, err)
		require.Contains(t, out, "mutually exclusive")
		require.Equal(t, before.Hash, paragraphHead(t, root).Hash)
		repo, err := gogit.PlainOpen(filepath.Join(root, ".mgit"))
		require.NoError(t, err)
		index, err := repo.Storer.Index()
		require.NoError(t, err)
		require.Empty(t, index.Entries, "refused commit must not stage pending files")
	}
}

func assertGitParagraphContract(t *testing.T) {
	t.Helper()
	root := t.TempDir()
	gitParagraphCommand(t, root, "init")
	gitParagraphCommand(t, root, "commit", "--allow-empty", "-m", "a", "-m", "b")
	repo, err := gogit.PlainOpen(root)
	require.NoError(t, err)
	head, err := repo.Head()
	require.NoError(t, err)
	commit, err := repo.CommitObject(head.Hash())
	require.NoError(t, err)
	require.Equal(t, "a\n\nb", strings.TrimSuffix(commit.Message, "\n"))
	file := filepath.Join(t.TempDir(), "message.txt")
	require.NoError(t, os.WriteFile(file, []byte("file message"), 0o600))
	cmd := exec.CommandContext(context.Background(), "git", "-C", root, "commit", "--allow-empty", "-m", "a", "-F", file) //nolint:gosec // fixed Git operation in a disposable test repository
	out, err := cmd.CombinedOutput()
	require.Error(t, err)
	require.Contains(t, string(out), "cannot be used together")
}

func paragraphHead(t *testing.T, root string) *object.Commit {
	t.Helper()
	repo, err := gogit.PlainOpen(filepath.Join(root, ".mgit"))
	require.NoError(t, err)
	head, err := repo.Head()
	require.NoError(t, err)
	commit, err := repo.CommitObject(head.Hash())
	require.NoError(t, err)
	return commit
}

func gitParagraphCommand(t *testing.T, root string, args ...string) {
	t.Helper()
	all := append([]string{"-C", root, "-c", "user.name=Test", "-c", "user.email=test@example.com", "-c", "commit.gpgsign=false"}, args...)
	cmd := exec.CommandContext(context.Background(), "git", all...) //nolint:gosec // test-controlled operations and disposable repository path
	out, err := cmd.CombinedOutput()
	require.NoError(t, err, string(out))
}
