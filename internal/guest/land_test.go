package guest

import (
	"bytes"
	"io"
	"os"
	"path/filepath"
	"testing"
	"time"

	gogit "github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/filemode"
	"github.com/go-git/go-git/v5/plumbing/object"
	"github.com/go-git/go-git/v5/storage/memory"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/hyper-swe/mgit/internal/landwire"
	"github.com/hyper-swe/mgit/internal/sandboxd/land"
)

// gitBuilder writes content-addressed objects into a memory store.
type gitBuilder struct {
	t  *testing.T
	st *memory.Storage
}

func newGitBuilder(t *testing.T) *gitBuilder {
	t.Helper()
	return &gitBuilder{t: t, st: memory.NewStorage()}
}

func (b *gitBuilder) blob(content string) plumbing.Hash {
	b.t.Helper()
	o := b.st.NewEncodedObject()
	o.SetType(plumbing.BlobObject)
	w, err := o.Writer()
	require.NoError(b.t, err)
	_, _ = w.Write([]byte(content))
	require.NoError(b.t, w.Close())
	h, err := b.st.SetEncodedObject(o)
	require.NoError(b.t, err)
	return h
}

func (b *gitBuilder) tree(entries ...object.TreeEntry) plumbing.Hash {
	b.t.Helper()
	o := b.st.NewEncodedObject()
	require.NoError(b.t, (&object.Tree{Entries: entries}).Encode(o))
	h, err := b.st.SetEncodedObject(o)
	require.NoError(b.t, err)
	return h
}

func (b *gitBuilder) commit(msg string, tree, parent plumbing.Hash) plumbing.Hash {
	b.t.Helper()
	sig := object.Signature{Name: "agent", Email: "a@mgit", When: time.Unix(0, 0).UTC()}
	gc := &object.Commit{Author: sig, Committer: sig, Message: msg, TreeHash: tree}
	if !parent.IsZero() {
		gc.ParentHashes = []plumbing.Hash{parent}
	}
	o := b.st.NewEncodedObject()
	require.NoError(b.t, gc.Encode(o))
	h, err := b.st.SetEncodedObject(o)
	require.NoError(b.t, err)
	return h
}

// TestGuestLandServer_StreamsObjects verifies the guest streams every object
// reachable from the branch head — the commit chain, trees, and blobs — in
// the landwire frame format the host decoder reads, exactly once each.
// Refs: FR-17.5, SEC-01
func TestGuestLandServer_StreamsObjects(t *testing.T) {
	b := newGitBuilder(t)
	blobA := b.blob("alpha")
	blobNested := b.blob("nested")
	sub := b.tree(object.TreeEntry{Name: "n.txt", Mode: filemode.Regular, Hash: blobNested})
	root1 := b.tree(
		object.TreeEntry{Name: "a.txt", Mode: filemode.Regular, Hash: blobA},
		object.TreeEntry{Name: "sub", Mode: filemode.Dir, Hash: sub},
	)
	base := b.commit("base", root1, plumbing.ZeroHash)
	// Second commit changes a.txt.
	blobA2 := b.blob("alpha v2")
	root2 := b.tree(
		object.TreeEntry{Name: "a.txt", Mode: filemode.Regular, Hash: blobA2},
		object.TreeEntry{Name: "sub", Mode: filemode.Dir, Hash: sub},
	)
	head := b.commit("edit", root2, base)

	var buf bytes.Buffer
	require.NoError(t, StreamReachable(b.st, head, &buf))

	// The host decoder reads the pool back; every object is present exactly once.
	objs, err := land.DefaultLimits().DecodeObjects(&buf)
	require.NoError(t, err)

	got := map[plumbing.Hash]byte{}
	for _, o := range objs {
		var typ plumbing.ObjectType
		switch o.Type {
		case landwire.ObjCommit:
			typ = plumbing.CommitObject
		case landwire.ObjTree:
			typ = plumbing.TreeObject
		case landwire.ObjBlob:
			typ = plumbing.BlobObject
		}
		h := plumbing.ComputeHash(typ, o.Data)
		_, dup := got[h]
		assert.False(t, dup, "object %s served more than once", h)
		got[h] = o.Type
	}

	for _, want := range []plumbing.Hash{base, head, root1, root2, sub, blobA, blobA2, blobNested} {
		assert.Contains(t, got, want, "object %s must be in the streamed pool", want)
	}
	assert.Equal(t, landwire.ObjCommit, got[head])
	assert.Equal(t, landwire.ObjTree, got[root2])
	assert.Equal(t, landwire.ObjBlob, got[blobA2])
}

func TestStreamReachable_EmptyHead_NoBytes(t *testing.T) {
	var buf bytes.Buffer
	require.NoError(t, StreamReachable(memory.NewStorage(), plumbing.ZeroHash, &buf))
	assert.Zero(t, buf.Len())
}

func TestStreamReachable_MissingObject_Error(t *testing.T) {
	// A head that is not in the store fails closed.
	err := StreamReachable(memory.NewStorage(), plumbing.ComputeHash(plumbing.CommitObject, []byte("absent")), io.Discard)
	assert.Error(t, err)
}

// TestServeLandHead_RealRepo streams a real on-disk repo's HEAD pool and
// confirms the host decoder reads the committed blob back (the guest open +
// resolve + stream path, cross-platform).
func TestServeLandHead_RealRepo(t *testing.T) {
	dir := t.TempDir()
	repo, err := gogit.PlainInit(dir, false)
	require.NoError(t, err)
	wt, err := repo.Worktree()
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(filepath.Join(dir, "f.txt"), []byte("land me"), 0o600))
	_, err = wt.Add("f.txt")
	require.NoError(t, err)
	_, err = wt.Commit("feat: f", &gogit.CommitOptions{
		Author: &object.Signature{Name: "agent", Email: "a@mgit", When: time.Unix(0, 0).UTC()},
	})
	require.NoError(t, err)

	var buf bytes.Buffer
	require.NoError(t, ServeLandHead(dir, &buf))
	objs, err := land.DefaultLimits().DecodeObjects(&buf)
	require.NoError(t, err)

	var foundBlob bool
	for _, o := range objs {
		if o.Type == landwire.ObjBlob && string(o.Data) == "land me" {
			foundBlob = true
		}
	}
	assert.True(t, foundBlob, "the committed blob is in the streamed pool")
}

func TestServeLandHead_UnbornHead_NoBytes(t *testing.T) {
	dir := t.TempDir()
	_, err := gogit.PlainInit(dir, false)
	require.NoError(t, err)
	var buf bytes.Buffer
	require.NoError(t, ServeLandHead(dir, &buf))
	assert.Zero(t, buf.Len(), "an empty repo serves nothing")
}

func TestServeLandHead_NotARepo_Error(t *testing.T) {
	assert.Error(t, ServeLandHead(t.TempDir(), io.Discard))
}
