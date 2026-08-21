package git

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/filemode"
	"github.com/go-git/go-git/v5/plumbing/object"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// A `.git` directory BELOW the repo root must never enter mgit's tree.
//
// The exclusion checked only the TOP-level component, so `.git` at the root
// was skipped correctly while `testdata/inner/.git/HEAD` — a vendored
// checkout, a sub-project, a git fixture under test data — was walked, hashed
// and absorbed into the base. The damage surfaced much later and somewhere
// else: every subsequent `mgit work` on that repo died with
//
//	materialize source: ... flatten tree: walk tree: invalid path component: ".git"
//
// because go-git refuses `.git` as a first or non-final path component. That
// guard is correct and fires as designed; what was wrong is that the content
// reached the tree at all.
//
// Reported as a /tmp-vs-home failure (a symlink theory). It is neither: the
// same repo reproduces identically under $HOME with no symlink in the path.
// Refs: MGIT-157, MGIT-14.7
func TestListWorkingFiles_NestedGitDirectory_IsNeverTracked(t *testing.T) {
	repo := initTestRepo(t)

	writeFile(t, repo, "a.txt", "project content\n")
	writeFile(t, repo, "testdata/inner/.git/HEAD", "ref: refs/heads/main\n")
	writeFile(t, repo, "testdata/inner/.git/config", "[core]\n")
	writeFile(t, repo, "testdata/inner/file.txt", "belongs to the other repo's worktree\n")
	writeFile(t, repo, "vendor/dep/.git/HEAD", "ref: refs/heads/main\n")

	paths, err := repo.listWorkingFiles()
	require.NoError(t, err)

	for _, p := range paths {
		assert.NotContains(t, p, ".git/",
			"a nested .git was tracked as project content: %s", p)
	}
	assert.Contains(t, paths, "a.txt", "ordinary content is still tracked")
	assert.Contains(t, paths, "testdata/inner/file.txt",
		"a sibling of a nested .git is still the project's own content")
}

// The same rule at the validator, so a tree that somehow carries such a path
// is refused by mgit with mgit's own words rather than by go-git deep inside a
// walk. Refs: MGIT-157
func TestValidateRelPath_RejectsAnExcludedDirAtAnyDepth(t *testing.T) {
	tests := []struct {
		name    string
		rel     string
		wantErr bool
	}{
		{name: "root_git", rel: ".git/config", wantErr: true},
		{name: "root_mgit", rel: ".mgit/index.db", wantErr: true},
		{name: "nested_git", rel: "testdata/inner/.git/HEAD", wantErr: true},
		{name: "deeply_nested_mgit", rel: "a/b/c/.mgit/objects/x", wantErr: true},
		{name: "gitfile_pointer", rel: "submodule/.git", wantErr: true},
		{name: "ordinary_path", rel: "internal/store/git/repository.go"},
		{name: "dotfile_that_is_not_git", rel: "a/.gitignore"},
		{name: "name_merely_containing_git", rel: "a/.github/workflows/ci.yml"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := validateRelPath(tt.rel)
			if tt.wantErr {
				require.Error(t, err, "%s must be refused", tt.rel)
				return
			}
			require.NoError(t, err, "%s is ordinary project content", tt.rel)
		})
	}
}

// End to end: the reproduction from the report. A repo carrying a nested .git
// materializes a worktree cleanly. Refs: MGIT-157
func TestMaterializeBranchTo_RepoWithANestedGit_Succeeds(t *testing.T) {
	repo := initTestRepo(t)
	ctx := context.Background()

	writeFile(t, repo, "a.txt", "x\n")
	writeFile(t, repo, "testdata/inner/.git/HEAD", "ref: refs/heads/main\n")
	writeFile(t, repo, "testdata/inner/file.txt", "y\n")

	paths, err := repo.listWorkingFiles()
	require.NoError(t, err)
	require.NoError(t, repo.stagePaths(paths))
	_, err = NewCommitStore(repo).CreateCommit(ctx, makeTestModelCommit(t, "MGIT-157"))
	require.NoError(t, err)

	head, err := repo.currentRef()
	require.NoError(t, err)
	branch := head.Name().Short()

	dest := filepath.Join(t.TempDir(), "wt")
	require.NoError(t, NewWorktreeStore(repo).MaterializeBranchTo(ctx, branch, dest),
		"a repo containing a nested .git must still provision a worktree")

	_, statErr := os.Stat(filepath.Join(dest, "testdata", "inner", "file.txt"))
	assert.NoError(t, statErr, "the sibling content is materialized")
	_, gitErr := os.Stat(filepath.Join(dest, "testdata", "inner", ".git"))
	assert.True(t, os.IsNotExist(gitErr), "the nested .git must not be materialized")
}

// An ALREADY-POISONED repo — one whose tree absorbed a nested .git before the
// exclusion was fixed — must be told what happened and what to do.
//
// The failure it produced was a chain of internal verbs ending in a go-git
// string: "materialize source: materialize branch task/X: flatten tree: walk
// tree: invalid path component: \".git\"". Every noun in it is an mgit
// implementation detail. A user cannot act on any of them, and the one fact
// that would help — your repository's recorded tree contains another
// repository's .git — appears nowhere. Refs: MGIT-157
func TestMaterializeBranchTo_TreeContainingDotGit_ExplainsItselfAndNamesTheRecourse(t *testing.T) {
	repo := initTestRepo(t)
	ctx := context.Background()

	// Build a tree carrying a nested .git directly, as a pre-fix store would.
	poisoned, err := writeBlob(repo.repo.Storer, []byte("ref: refs/heads/main\n"))
	require.NoError(t, err)
	ok, err := writeBlob(repo.repo.Storer, []byte("fine\n"))
	require.NoError(t, err)
	files := map[string]blobEntry{
		"a.txt":                    {hash: ok, mode: filemode.Regular},
		"testdata/inner/.git/HEAD": {hash: poisoned, mode: filemode.Regular},
	}
	treeHash, err := writeNestedTree(repo.repo.Storer, files)
	require.NoError(t, err)

	head, err := repo.currentRef()
	require.NoError(t, err)
	sig := object.Signature{Name: "t", Email: "t@t", When: time.Unix(0, 0).UTC()}
	c := &object.Commit{Author: sig, Committer: sig, TreeHash: treeHash, Message: "poisoned"}
	obj := repo.repo.Storer.NewEncodedObject()
	require.NoError(t, c.Encode(obj))
	commitHash, err := repo.repo.Storer.SetEncodedObject(obj)
	require.NoError(t, err)
	require.NoError(t, repo.repo.Storer.SetReference(
		plumbing.NewHashReference(head.Name(), commitHash)))

	err = NewWorktreeStore(repo).MaterializeBranchTo(ctx, head.Name().Short(), filepath.Join(t.TempDir(), "wt"))
	require.Error(t, err)
	msg := err.Error()

	// The NESTED REPOSITORY is named, not one file inside it: "your tree
	// contains testdata/inner/.git" is what a user can act on, where
	// "…/.git/HEAD" would send them after a single file.
	assert.Contains(t, msg, "testdata/inner/.git",
		"the message must name the offending path, not just its last component")
	assert.Contains(t, msg, "another repository",
		"the message must say WHAT the path is, in the user's terms")
	assert.NotContains(t, msg, "flatten tree",
		"an internal verb chain is not a diagnosis")
	assert.NotContains(t, msg, "walk tree",
		"an internal verb chain is not a diagnosis")

	// The recourse must be one a user can follow AND one that is true. An
	// earlier draft told them to remove the nested repository; verified
	// against a real poisoned repo, re-recording the base alone is enough,
	// and deleting a checkout they may need would have been wrong advice.
	assert.Contains(t, msg, "mgit init", "the message must name the way out")
	assert.Contains(t, msg, "can stay", "the nested repository does not have to be deleted")
}
