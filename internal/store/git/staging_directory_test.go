package git

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/hyper-swe/mgit/internal/model"
)

// MGIT-276: `mgit add <dir>` stored the directory itself as one staged path,
// after which every commit failed reading it as a file and nothing could
// unstage it.

func stagedNow(t *testing.T, repo *Repository) []string {
	t.Helper()
	paths, err := repo.stagedPaths()
	require.NoError(t, err)
	return paths
}

func writeFiles(t *testing.T, repo *Repository, files map[string]string) {
	t.Helper()
	for rel, content := range files {
		p := filepath.Join(repo.Root(), rel)
		require.NoError(t, os.MkdirAll(filepath.Dir(p), 0o750))
		require.NoError(t, os.WriteFile(p, []byte(content), 0o600))
	}
}

func TestAdd_Directory_StagesEveryChangedFileUnderItNeverTheDirectory(t *testing.T) {
	repo := initTestRepo(t)
	writeAndCommit(t, repo, "MGIT-1", map[string]string{
		"pkg/tr/keep.go": "v1\n", "pkg/tr/old.go": "old\n", "pkg/trx.go": "x\n", "other.go": "o\n",
	})
	writeFiles(t, repo, map[string]string{
		"pkg/tr/keep.go": "v2\n", "pkg/tr/sub/new.go": "new\n", "pkg/trx.go": "changed\n", "other.go": "changed\n",
	})
	require.NoError(t, os.Remove(filepath.Join(repo.Root(), "pkg", "tr", "old.go")))

	for _, arg := range []string{"pkg/tr", "pkg/tr/"} {
		require.NoError(t, repo.clearStaging())
		require.NoError(t, NewWorktreeStore(repo).Add(context.Background(), arg), arg)
		assert.Equal(t, []string{"pkg/tr/keep.go", "pkg/tr/old.go", "pkg/tr/sub/new.go"}, stagedNow(t, repo),
			"%s: the changed, deleted and new files under it; not a sibling sharing its prefix", arg)
	}
}

func TestAdd_UnchangedDirectory_IsANoOp(t *testing.T) {
	repo := initTestRepo(t)
	writeAndCommit(t, repo, "MGIT-1", map[string]string{"pkg/a.go": "a\n"})
	require.NoError(t, os.MkdirAll(filepath.Join(repo.Root(), "empty"), 0o750))

	ws := NewWorktreeStore(repo)
	require.NoError(t, ws.Add(context.Background(), "pkg"))
	require.NoError(t, ws.Add(context.Background(), "empty"))
	assert.Empty(t, stagedNow(t, repo))
}

func TestAdd_DeletedDirectory_StagesTheDeletionOfEveryTrackedFileUnderIt(t *testing.T) {
	repo := initTestRepo(t)
	writeAndCommit(t, repo, "MGIT-1", map[string]string{"pkg/a.go": "a\n", "pkg/b/c.go": "c\n", "keep.go": "k\n"})
	require.NoError(t, os.RemoveAll(filepath.Join(repo.Root(), "pkg")))

	require.NoError(t, NewWorktreeStore(repo).Add(context.Background(), "pkg"))
	assert.Equal(t, []string{"pkg/a.go", "pkg/b/c.go"}, stagedNow(t, repo))
}

func TestAdd_Directory_SkipsIgnoredFiles(t *testing.T) {
	repo := initTestRepo(t)
	writeAndCommit(t, repo, "MGIT-1", map[string]string{".gitignore": "*.log\n"})
	writeFiles(t, repo, map[string]string{"pkg/run.log": "noise\n", "pkg/y.go": "y\n"})

	require.NoError(t, NewWorktreeStore(repo).Add(context.Background(), "pkg"))
	assert.Equal(t, []string{"pkg/y.go"}, stagedNow(t, repo))
}

// A staging file written by mgit 0.6.8 can still name a directory. Commit
// refuses it by name, with the way out, instead of failing to read it.
func TestCommit_StagedDirectoryEntry_RefusedNamingItAndTheWayOut(t *testing.T) {
	repo := initTestRepo(t)
	writeFiles(t, repo, map[string]string{"pkg/tr/a.go": "a\n", "other.go": "o\n"})
	require.NoError(t, repo.saveStaging(&stagingState{Paths: []string{"other.go", "pkg/tr"}}))

	_, err := NewCommitStore(repo).CreateCommit(context.Background(), makeTestModelCommit(t, "MGIT-1"))
	require.Error(t, err)
	assert.True(t, errors.Is(err, model.ErrInvalidStagedEntry), "got %v", err)
	assert.Contains(t, err.Error(), "pkg/tr")
	assert.Contains(t, err.Error(), "mgit restore --staged pkg/tr")
	assert.NotContains(t, err.Error(), "read working file")
}

func TestCommit_StagedEntryForADeletedTrackedDirectory_Refused(t *testing.T) {
	repo := initTestRepo(t)
	writeAndCommit(t, repo, "MGIT-1", map[string]string{"pkg/a.go": "a\n"})
	require.NoError(t, os.RemoveAll(filepath.Join(repo.Root(), "pkg")))
	require.NoError(t, repo.saveStaging(&stagingState{Paths: []string{"pkg"}}))

	_, err := NewCommitStore(repo).CreateCommit(context.Background(), makeTestModelCommit(t, "MGIT-1"))
	assert.True(t, errors.Is(err, model.ErrInvalidStagedEntry),
		"a directory entry would otherwise commit nothing for the files under it; got %v", err)
}

func TestUnstage_NamedPaths_RemovesExactlyThose(t *testing.T) {
	repo := initTestRepo(t)
	require.NoError(t, repo.saveStaging(&stagingState{Paths: []string{"a.go", "b.go", "c.go"}}))

	removed, err := repo.Unstage([]string{"b.go"})
	require.NoError(t, err)
	assert.Equal(t, []string{"b.go"}, removed)
	assert.Equal(t, []string{"a.go", "c.go"}, stagedNow(t, repo))
}

func TestUnstage_Directory_RemovesItsOwnEntryAndEverythingUnderIt(t *testing.T) {
	repo := initTestRepo(t)
	require.NoError(t, repo.saveStaging(&stagingState{Paths: []string{"pkg", "pkg/a.go", "pkg/b/c.go", "pkga.go", "x.go"}}))

	removed, err := repo.Unstage([]string{"pkg/"})
	require.NoError(t, err)
	assert.Equal(t, []string{"pkg", "pkg/a.go", "pkg/b/c.go"}, removed)
	assert.Equal(t, []string{"pkga.go", "x.go"}, stagedNow(t, repo))
}

func TestUnstage_PathNotStaged_RemovesNothing(t *testing.T) {
	repo := initTestRepo(t)
	require.NoError(t, repo.saveStaging(&stagingState{Paths: []string{"a.go"}}))

	removed, err := repo.Unstage([]string{"zzz.go"})
	require.NoError(t, err)
	assert.Empty(t, removed)
	assert.Equal(t, []string{"a.go"}, stagedNow(t, repo))
}

func TestUnstage_EscapingPath_Refused(t *testing.T) {
	repo := initTestRepo(t)
	_, err := repo.Unstage([]string{"../outside"})
	require.Error(t, err)
}

// treeKeys is the set of paths in HEAD's tree after a commit.
func treeKeys(t *testing.T, repo *Repository) map[string]bool {
	t.Helper()
	head, err := repo.headFiles()
	require.NoError(t, err)
	keys := map[string]bool{}
	for k := range head {
		keys[k] = true
	}
	return keys
}

// assertNoFileDirClash fails when a tree holds a file and a directory under
// one name, the "duplicateEntries" git fsck reports.
func assertNoFileDirClash(t *testing.T, keys map[string]bool) {
	t.Helper()
	for k := range keys {
		for dir := filepath.Dir(k); dir != "." && dir != "/"; dir = filepath.Dir(dir) {
			assert.False(t, keys[filepath.ToSlash(dir)], "%s is both a file and a directory of %s", dir, k)
		}
	}
}

// THE REVIEW'S CASE. A tracked file replaced by a directory of the same name:
// `add x` stages the file's deletion as well as the files under the new
// directory, so the commit records the directory and no stale file.
func TestAdd_FileReplacedByDirectory_StagesTheFilesDeletionToo(t *testing.T) {
	repo := initTestRepo(t)
	writeAndCommit(t, repo, "MGIT-1", map[string]string{"x": "file\n"})
	require.NoError(t, os.Remove(filepath.Join(repo.Root(), "x")))
	writeFiles(t, repo, map[string]string{"x/z": "z\n"})

	require.NoError(t, NewWorktreeStore(repo).Add(context.Background(), "x"))
	assert.Equal(t, []string{"x", "x/z"}, stagedNow(t, repo))
	_, err := NewCommitStore(repo).CreateCommit(context.Background(), makeTestModelCommit(t, "MGIT-1"))
	require.NoError(t, err)
	keys := treeKeys(t, repo)
	assert.True(t, keys["x/z"])
	assert.False(t, keys["x"])
	assertNoFileDirClash(t, keys)
}

// The older root of the same defect: staging only the new file under the
// directory replaces the stale file, as `git add x/z` does.
func TestCommit_StagedPathUnderAReplacedFile_ReplacesTheFile(t *testing.T) {
	repo := initTestRepo(t)
	writeAndCommit(t, repo, "MGIT-1", map[string]string{"x": "file\n", "keep.go": "k\n"})
	require.NoError(t, os.Remove(filepath.Join(repo.Root(), "x")))
	writeFiles(t, repo, map[string]string{"x/z": "z\n"})
	require.NoError(t, NewWorktreeStore(repo).Add(context.Background(), "x/z"))

	_, err := NewCommitStore(repo).CreateCommit(context.Background(), makeTestModelCommit(t, "MGIT-1"))
	require.NoError(t, err)
	keys := treeKeys(t, repo)
	assert.True(t, keys["x/z"])
	assert.True(t, keys["keep.go"])
	assertNoFileDirClash(t, keys)
}

// The other direction: a tracked directory replaced by a file of the same
// name. Staging the file replaces the directory's entries.
func TestCommit_DirectoryReplacedByFile_ReplacesItsEntries(t *testing.T) {
	repo := initTestRepo(t)
	writeAndCommit(t, repo, "MGIT-1", map[string]string{"x/a": "a\n", "x/b/c": "c\n"})
	require.NoError(t, os.RemoveAll(filepath.Join(repo.Root(), "x")))
	writeFiles(t, repo, map[string]string{"x": "now a file\n"})
	require.NoError(t, NewWorktreeStore(repo).Add(context.Background(), "x"))

	_, err := NewCommitStore(repo).CreateCommit(context.Background(), makeTestModelCommit(t, "MGIT-1"))
	require.NoError(t, err)
	keys := treeKeys(t, repo)
	assert.True(t, keys["x"])
	assert.False(t, keys["x/a"])
	assert.False(t, keys["x/b/c"])
	assertNoFileDirClash(t, keys)
}
