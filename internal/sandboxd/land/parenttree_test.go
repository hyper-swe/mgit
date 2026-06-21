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

	"github.com/hyper-swe/mgit/internal/model"
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

// TestPoolAwareParentResolver_IntraBatchParentFromPool verifies a parent that
// is a new commit in the batch resolves from the registered pool (it is not
// yet in the host store), enabling multi-commit chains. Refs: FR-17.5, SEC-06
func TestPoolAwareParentResolver_IntraBatchParentFromPool(t *testing.T) {
	b := newBuilder(t)
	blob := b.writeBlob("v1")
	tree := b.writeTree(object.TreeEntry{Name: "a.txt", Mode: filemode.Regular, Hash: blob})
	parent := b.writeCommit("c1", "a", tree, plumbing.ZeroHash, time.Unix(0, 0).UTC())
	pool := []Object{
		{Type: ObjBlob, Data: b.raw(blob)},
		{Type: ObjTree, Data: b.raw(tree)},
		{Type: ObjCommit, Data: b.raw(parent)},
	}

	r := NewPoolAwareParentResolver(NewHostParentTreeResolver(openRepo(t)))
	ids, err := r.Register(pool)
	require.NoError(t, err)
	require.Contains(t, ids, parent.String())

	files, err := r.ParentFileSet(context.Background(), parent.String())
	require.NoError(t, err)
	assert.Equal(t, map[string]string{"a.txt": blob.String()}, files)

	r.Deregister(ids)
	// After deregister it falls through to the host store, where the commit is
	// absent → error.
	_, err = r.ParentFileSet(context.Background(), parent.String())
	assert.Error(t, err)
}

// TestPoolAwareParentResolver_BaseParentFromHost verifies a parent already in
// the host store (base history) resolves via the wrapped host resolver.
func TestPoolAwareParentResolver_BaseParentFromHost(t *testing.T) {
	repo := openRepo(t)
	b := newBuilder(t)
	blob := b.writeBlob("base")
	tree := b.writeTree(object.TreeEntry{Name: "base.txt", Mode: filemode.Regular, Hash: blob})
	commit := b.writeCommit("base", "a", tree, plumbing.ZeroHash, time.Unix(0, 0).UTC())
	for _, o := range []struct {
		typ plumbing.ObjectType
		h   plumbing.Hash
	}{{plumbing.BlobObject, blob}, {plumbing.TreeObject, tree}, {plumbing.CommitObject, commit}} {
		_, err := repo.WriteRawObject(o.typ, b.raw(o.h))
		require.NoError(t, err)
	}

	r := NewPoolAwareParentResolver(NewHostParentTreeResolver(repo))
	files, err := r.ParentFileSet(context.Background(), commit.String())
	require.NoError(t, err)
	assert.Equal(t, map[string]string{"base.txt": blob.String()}, files)
}

func TestPoolAwareParentResolver_EmptyParent(t *testing.T) {
	r := NewPoolAwareParentResolver(NewHostParentTreeResolver(openRepo(t)))
	files, err := r.ParentFileSet(context.Background(), "")
	require.NoError(t, err)
	assert.Empty(t, files)
}

func TestPoolAwareParentResolver_DuplicateCommit_RegisterError(t *testing.T) {
	b := newBuilder(t)
	tree := b.writeTree(object.TreeEntry{Name: "a.txt", Mode: filemode.Regular, Hash: b.writeBlob("x")})
	c := b.writeCommit("c", "a", tree, plumbing.ZeroHash, time.Unix(0, 0).UTC())
	pool := []Object{{Type: ObjCommit, Data: b.raw(c)}, {Type: ObjCommit, Data: b.raw(c)}}
	r := NewPoolAwareParentResolver(NewHostParentTreeResolver(openRepo(t)))
	_, err := r.Register(pool)
	assert.ErrorIs(t, err, model.ErrLandVerificationFailed)
}

// TestPoolAwareParentResolver_IntraBatchTreeMissing_Error verifies an
// intra-batch parent whose tree is absent from the pool fails closed.
func TestPoolAwareParentResolver_IntraBatchTreeMissing_Error(t *testing.T) {
	b := newBuilder(t)
	tree := b.writeTree(object.TreeEntry{Name: "a.txt", Mode: filemode.Regular, Hash: b.writeBlob("x")})
	parent := b.writeCommit("c1", "a", tree, plumbing.ZeroHash, time.Unix(0, 0).UTC())
	// Pool has the commit but NOT its tree object.
	pool := []Object{{Type: ObjCommit, Data: b.raw(parent)}}
	r := NewPoolAwareParentResolver(NewHostParentTreeResolver(openRepo(t)))
	ids, err := r.Register(pool)
	require.NoError(t, err)
	require.Contains(t, ids, parent.String())
	_, err = r.ParentFileSet(context.Background(), parent.String())
	assert.ErrorIs(t, err, model.ErrLandVerificationFailed)
}
