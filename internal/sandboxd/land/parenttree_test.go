package land

import (
	"context"
	"testing"
	"time"

	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/filemode"
	"github.com/go-git/go-git/v5/plumbing/object"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	gitstore "github.com/hyper-swe/mgit/internal/store/git"
)

func openRepo(t *testing.T) *gitstore.Repository {
	t.Helper()
	repo, err := gitstore.Init(t.TempDir(), func() time.Time { return time.Unix(0, 0).UTC() })
	require.NoError(t, err)
	t.Cleanup(func() { _ = repo.Close() })
	return repo
}

// TestHostParentTreeResolver_EmptyParent_EmptySet verifies an initial commit
// (no parent) resolves to the empty file set.
func TestHostParentTreeResolver_EmptyParent_EmptySet(t *testing.T) {
	r := NewHostParentTreeResolver(openRepo(t))
	files, err := r.ParentFileSet(context.Background(), "")
	require.NoError(t, err)
	assert.Empty(t, files)

	files, err = r.ParentFileSet(context.Background(), plumbing.ZeroHash.String())
	require.NoError(t, err)
	assert.Empty(t, files)
}

// TestHostParentTreeResolver_ResolvesLeafFiles verifies a commit's nested
// tree resolves to leaf path -> blob hash, excluding directory nodes.
func TestHostParentTreeResolver_ResolvesLeafFiles(t *testing.T) {
	repo := openRepo(t)
	b := newBuilder(t)
	blobRoot := b.writeBlob("root file")
	blobNested := b.writeBlob("nested file")
	subtree := b.writeTree(object.TreeEntry{Name: "nested.txt", Mode: filemode.Regular, Hash: blobNested})
	root := b.writeTree(
		object.TreeEntry{Name: "a.txt", Mode: filemode.Regular, Hash: blobRoot},
		object.TreeEntry{Name: "sub", Mode: filemode.Dir, Hash: subtree},
	)
	commit := b.writeCommit("feat: tree", "agent", root, plumbing.ZeroHash, time.Unix(0, 0).UTC())

	// Import the objects into the host store, then resolve.
	for _, o := range []struct {
		typ  plumbing.ObjectType
		hash plumbing.Hash
	}{
		{plumbing.BlobObject, blobRoot}, {plumbing.BlobObject, blobNested},
		{plumbing.TreeObject, subtree}, {plumbing.TreeObject, root},
		{plumbing.CommitObject, commit},
	} {
		_, err := repo.WriteRawObject(o.typ, b.raw(o.hash))
		require.NoError(t, err)
	}

	r := NewHostParentTreeResolver(repo)
	files, err := r.ParentFileSet(context.Background(), commit.String())
	require.NoError(t, err)
	assert.Equal(t, map[string]string{
		"a.txt":          blobRoot.String(),
		"sub/nested.txt": blobNested.String(),
	}, files)
}

func TestHostParentTreeResolver_UnknownParent_Error(t *testing.T) {
	r := NewHostParentTreeResolver(openRepo(t))
	_, err := r.ParentFileSet(context.Background(), plumbing.ComputeHash(plumbing.CommitObject, []byte("absent")).String())
	assert.Error(t, err)
}
