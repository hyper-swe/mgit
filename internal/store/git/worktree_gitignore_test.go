package git

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// writeFileMk writes a worktree file, creating parent dirs, for the gitignore
// tests. Refs: MGIT-32
func writeFileMk(t *testing.T, root, rel, content string) {
	t.Helper()
	abs := filepath.Join(root, filepath.FromSlash(rel))
	require.NoError(t, os.MkdirAll(filepath.Dir(abs), 0o750))
	require.NoError(t, os.WriteFile(abs, []byte(content), 0o600))
}

// TestWorktreeStore_AddAll_HonorsGitignore proves `mgit add .` does NOT stage
// paths the repo's .gitignore excludes (ignored file, ignored dir, glob), while
// still staging tracked-candidate files and the .gitignore itself. Refs: MGIT-32
func TestWorktreeStore_AddAll_HonorsGitignore(t *testing.T) {
	repo := initTestRepo(t)
	ctx := context.Background()
	ws := NewWorktreeStore(repo)

	writeFileMk(t, repo.Root(), ".gitignore", "ignored.txt\nbuild/\n*.log\n")
	writeFileMk(t, repo.Root(), "a.go", "package a\n")
	writeFileMk(t, repo.Root(), "ignored.txt", "secret\n")
	writeFileMk(t, repo.Root(), "build/out.o", "junk\n")
	writeFileMk(t, repo.Root(), "app.log", "log\n")

	require.NoError(t, ws.Add(ctx, "."))
	staged, err := repo.stagedPaths()
	require.NoError(t, err)
	assert.ElementsMatch(t, []string{".gitignore", "a.go"}, staged,
		"add . must skip .gitignore-excluded paths (ignored.txt, build/, *.log)")
}

// TestWorktreeStore_Status_HonorsGitignore proves `mgit status` does not list
// .gitignore-excluded untracked paths. Refs: MGIT-32
func TestWorktreeStore_Status_HonorsGitignore(t *testing.T) {
	repo := initTestRepo(t)
	ctx := context.Background()
	ws := NewWorktreeStore(repo)

	writeFileMk(t, repo.Root(), ".gitignore", "ignored.txt\nbuild/\n")
	writeFileMk(t, repo.Root(), "a.go", "package a\n")
	writeFileMk(t, repo.Root(), "ignored.txt", "secret\n")
	writeFileMk(t, repo.Root(), "build/out.o", "junk\n")

	files, err := ws.Status(ctx)
	require.NoError(t, err)
	_, hasA := statusFor(files, "a.go")
	_, hasIgnored := statusFor(files, "ignored.txt")
	_, hasBuild := statusFor(files, "build/out.o")
	assert.True(t, hasA, "a non-ignored untracked file is listed")
	assert.False(t, hasIgnored, "an ignored file is not listed as untracked")
	assert.False(t, hasBuild, "a file under an ignored dir is not listed")
}

// TestWorktreeStore_Gitignore_NestedAndNegation proves a nested .gitignore and a
// negation (!) are honored. Refs: MGIT-32
func TestWorktreeStore_Gitignore_NestedAndNegation(t *testing.T) {
	repo := initTestRepo(t)
	ctx := context.Background()
	ws := NewWorktreeStore(repo)

	writeFileMk(t, repo.Root(), ".gitignore", "*.log\n")
	writeFileMk(t, repo.Root(), "sub/.gitignore", "!keep.log\n")
	writeFileMk(t, repo.Root(), "sub/keep.log", "kept\n")
	writeFileMk(t, repo.Root(), "sub/drop.log", "dropped\n")

	require.NoError(t, ws.Add(ctx, "."))
	staged, err := repo.stagedPaths()
	require.NoError(t, err)
	assert.Contains(t, staged, "sub/keep.log", "a negated path is re-included")
	assert.NotContains(t, staged, "sub/drop.log", "a nested-ignored path stays excluded")
}

// TestWorktreeStore_Gitignore_AlwaysSkipsGitAndMgit proves .git/ and .mgit/ are
// never staged regardless of .gitignore (mgit's store + the project git repo are
// sacrosanct). Refs: MGIT-32, MGIT-14
func TestWorktreeStore_Gitignore_AlwaysSkipsGitAndMgit(t *testing.T) {
	repo := initTestRepo(t)
	ctx := context.Background()
	ws := NewWorktreeStore(repo)

	// A project .git with content + a .gitignore that does not mention it.
	writeFileMk(t, repo.Root(), ".git/config", "[core]\n")
	writeFileMk(t, repo.Root(), ".gitignore", "*.tmp\n")
	writeFileMk(t, repo.Root(), "real.go", "package real\n")

	require.NoError(t, ws.Add(ctx, "."))
	staged, err := repo.stagedPaths()
	require.NoError(t, err)
	for _, p := range staged {
		assert.NotContains(t, p, ".git/", "the project .git is never staged")
		assert.NotContains(t, p, ".mgit/", "mgit's own store is never staged")
	}
	assert.Contains(t, staged, "real.go")
}
