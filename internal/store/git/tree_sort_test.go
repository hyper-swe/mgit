package git

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/go-git/go-git/v5/plumbing/filemode"
	"github.com/go-git/go-git/v5/plumbing/object"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestCreateCommit_MultiEntryTree_DirFilePrefix is the regression for the bug
// dogfooding surfaced: committing a real multi-file tree failed with "entries
// in tree are not sorted" because tree entries were sorted by plain name, not
// git's canonical order (a directory sorts as if its name had a trailing "/").
// The trigger is a dir/file prefix sibling pair — dir "land" vs file "land.go":
// git orders "land.go" before "land/". Every prior test committed only a
// single-entry tree, so the suite never exercised this. Refs: MGIT-14
func TestCreateCommit_MultiEntryTree_DirFilePrefix(t *testing.T) {
	repo := initTestRepo(t)
	ws := NewWorktreeStore(repo)
	cs := NewCommitStore(repo)
	ctx := context.Background()
	root := repo.Root()

	// A dir/file prefix collision at the root level, plus extra siblings so
	// the tree has many entries.
	require.NoError(t, os.WriteFile(filepath.Join(root, "land.go"), []byte("file\n"), 0o600))
	require.NoError(t, os.MkdirAll(filepath.Join(root, "land"), 0o750))
	require.NoError(t, os.WriteFile(filepath.Join(root, "land", "inner.go"), []byte("inner\n"), 0o600))
	require.NoError(t, os.WriteFile(filepath.Join(root, "a.go"), []byte("a\n"), 0o600))
	require.NoError(t, os.WriteFile(filepath.Join(root, "z.go"), []byte("z\n"), 0o600))

	require.NoError(t, ws.Add(ctx, "."))
	c := makeTestModelCommit(t, "MGIT-14")
	h, err := cs.CreateCommit(ctx, c)
	require.NoError(t, err, "committing a multi-entry tree with a dir/file prefix pair must succeed (git-canonical tree sort)")
	require.NotEmpty(t, h)

	// The committed content round-trips at both the root level and the subtree.
	got, err := cs.GetFileFromCommit(ctx, h, "land.go")
	require.NoError(t, err)
	assert.Equal(t, "file\n", string(got))
	got, err = cs.GetFileFromCommit(ctx, h, "land/inner.go")
	require.NoError(t, err)
	assert.Equal(t, "inner\n", string(got))
}

// TestTreeEntryLess_GitCanonicalOrder pins the comparator: a directory sorts
// as if its name had a trailing "/", so file "land.go" precedes dir "land".
func TestTreeEntryLess_GitCanonicalOrder(t *testing.T) {
	dir := object.TreeEntry{Name: "land", Mode: filemode.Dir}
	file := object.TreeEntry{Name: "land.go", Mode: filemode.Regular}
	assert.True(t, treeEntryLess(file, dir), `"land.go" must sort before dir "land/"`)
	assert.False(t, treeEntryLess(dir, file))

	// Plain siblings keep byte order; a dir still trails a same-stem file.
	a := object.TreeEntry{Name: "a.go", Mode: filemode.Regular}
	z := object.TreeEntry{Name: "z.go", Mode: filemode.Regular}
	assert.True(t, treeEntryLess(a, z))
}
