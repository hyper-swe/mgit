package gitref

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	gogit "github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/plumbing/object"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// A new task's base takes git's committed tree without the contents of a
// store nested below the root; the generic reader of committed files keeps
// reporting everything git committed. Refs: MGIT-283

func gitRepoWithFiles(t *testing.T, files map[string]string) string {
	t.Helper()
	dir := t.TempDir()
	repo, err := gogit.PlainInit(dir, false)
	require.NoError(t, err)
	wt, err := repo.Worktree()
	require.NoError(t, err)
	for rel, content := range files {
		abs := filepath.Join(dir, filepath.FromSlash(rel))
		require.NoError(t, os.MkdirAll(filepath.Dir(abs), 0o750))
		require.NoError(t, os.WriteFile(abs, []byte(content), 0o600))
		_, err = wt.Add(rel)
		require.NoError(t, err)
	}
	sig := &object.Signature{Name: "dev", Email: "dev@x", When: time.Unix(0, 0).UTC()}
	_, err = wt.Commit("seed", &gogit.CommitOptions{Author: sig, Committer: sig})
	require.NoError(t, err)
	return dir
}

func paths(files []CommittedFile) []string {
	out := make([]string, 0, len(files))
	for _, f := range files {
		out = append(out, f.Path)
	}
	return out
}

func TestCommittedSnapshotFiles_LeavesOutNestedStoreAndNamesItOnce(t *testing.T) {
	root := gitRepoWithFiles(t, map[string]string{
		"main.go":             "package main\n",
		"archive/keep.go":     "package archive\n",
		"archive/.mgit/a":     "x\n",
		"archive/.mgit/d/e":   "x\n",
		"p/q/r/.mgit/objects": "x\n",
	})

	snap, err := CommittedSnapshotFiles(root)

	require.NoError(t, err)
	assert.ElementsMatch(t, []string{"main.go", "archive/keep.go"}, paths(snap.Files))
	assert.Equal(t, []string{"archive/.mgit", "p/q/r/.mgit"}, snap.Skipped)
	assert.Len(t, snap.Head, 40)
}

// The generic reader is unchanged: it still reports every committed file.
func TestCommittedFiles_StillReportsNestedStoreFiles(t *testing.T) {
	root := gitRepoWithFiles(t, map[string]string{
		"main.go":         "package main\n",
		"archive/.mgit/a": "x\n",
	})

	files, _, err := CommittedFiles(root)

	require.NoError(t, err)
	assert.ElementsMatch(t, []string{"main.go", "archive/.mgit/a"}, paths(files))
}

// What the rule does not match: the root store, a name that only starts with
// `.mgit`, and a final component named `.mgit`.
func TestCommittedSnapshotFiles_OnlyANestedMgitDirectoryIsSkipped(t *testing.T) {
	root := gitRepoWithFiles(t, map[string]string{
		".mgit/rootfile":  "root store\n",
		"x/.mgitish/data": "kept\n",
		"x/.mgit.bak":     "kept\n",
		"y/.mgit":         "a file named like the store\n",
	})

	snap, err := CommittedSnapshotFiles(root)

	require.NoError(t, err)
	assert.ElementsMatch(t, []string{".mgit/rootfile", "x/.mgitish/data", "x/.mgit.bak", "y/.mgit"}, paths(snap.Files))
	assert.Empty(t, snap.Skipped)
}

func TestCommittedSnapshotFiles_NoGit_ReturnsErrNoGit(t *testing.T) {
	_, err := CommittedSnapshotFiles(t.TempDir())
	assert.ErrorIs(t, err, ErrNoGit)
}

func TestNestedMgitFolder(t *testing.T) {
	tests := []struct {
		path   string
		folder string
		ok     bool
	}{
		{"archive/.mgit/obj", "archive/.mgit", true},
		{"a/b/c/.mgit/x/y", "a/b/c/.mgit", true},
		{".mgit/obj", "", false},
		{"a/.mgit", "", false},
		{"a/.mgitish/x", "", false},
		{"a/x.mgit/y", "", false},
		{"main.go", "", false},
	}
	for _, tc := range tests {
		t.Run(tc.path, func(t *testing.T) {
			folder, ok := nestedMgitFolder(tc.path)
			assert.Equal(t, tc.ok, ok)
			assert.Equal(t, tc.folder, folder)
		})
	}
}
