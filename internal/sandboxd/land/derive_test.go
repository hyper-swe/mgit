package land

import (
	"io"
	"testing"
	"time"

	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/filemode"
	"github.com/go-git/go-git/v5/plumbing/object"
	"github.com/go-git/go-git/v5/storage/memory"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/hyper-swe/mgit/internal/model"
)

// builder accumulates content-addressed git objects in an in-memory store
// and exposes their raw bytes, so tests can assemble realistic land pools.
type builder struct {
	t  *testing.T
	st *memory.Storage
}

func newBuilder(t *testing.T) *builder {
	t.Helper()
	return &builder{t: t, st: memory.NewStorage()}
}

// raw returns an object's canonical payload bytes (no git header), the form
// carried in a land frame and consumed by CommitFromObjectData.
func (b *builder) raw(h plumbing.Hash) []byte {
	b.t.Helper()
	o, err := b.st.EncodedObject(plumbing.AnyObject, h)
	require.NoError(b.t, err)
	r, err := o.Reader()
	require.NoError(b.t, err)
	data, err := io.ReadAll(r)
	require.NoError(b.t, err)
	return data
}

func (b *builder) writeBlob(content string) plumbing.Hash {
	b.t.Helper()
	o := b.st.NewEncodedObject()
	o.SetType(plumbing.BlobObject)
	w, err := o.Writer()
	require.NoError(b.t, err)
	_, err = w.Write([]byte(content))
	require.NoError(b.t, err)
	require.NoError(b.t, w.Close())
	h, err := b.st.SetEncodedObject(o)
	require.NoError(b.t, err)
	return h
}

func (b *builder) writeTree(entries ...object.TreeEntry) plumbing.Hash {
	b.t.Helper()
	tree := &object.Tree{Entries: entries}
	o := b.st.NewEncodedObject()
	require.NoError(b.t, tree.Encode(o))
	h, err := b.st.SetEncodedObject(o)
	require.NoError(b.t, err)
	return h
}

func (b *builder) writeCommit(msg, agent string, tree, parent plumbing.Hash, when time.Time) plumbing.Hash {
	b.t.Helper()
	sig := object.Signature{Name: agent, Email: agent + "@mgit", When: when}
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

func taskID(t *testing.T, s string) model.TaskID {
	t.Helper()
	id, err := model.ParseTaskID(s)
	require.NoError(t, err)
	return id
}

// TestDeriveLandedCommit_BindsTreeDiffAndContentHash verifies a derived
// commit passes both host bindings by construction: its FileDiffs match the
// landed tree and its content_hash is self-consistent. Refs: FR-17.24, SEC-06
func TestDeriveLandedCommit_BindsTreeDiffAndContentHash(t *testing.T) {
	b := newBuilder(t)
	when := time.Date(2026, 6, 21, 9, 0, 0, 0, time.UTC)
	blob := b.writeBlob("hello land")
	tree := b.writeTree(object.TreeEntry{Name: "a.txt", Mode: filemode.Regular, Hash: blob})
	commit := b.writeCommit("feat: add a.txt", "agent-7", tree, plumbing.ZeroHash, when)

	pool := []Object{
		{Type: ObjBlob, Data: b.raw(blob)},
		{Type: ObjTree, Data: b.raw(tree)},
		{Type: ObjCommit, Data: b.raw(commit)},
	}

	c, err := DeriveLandedCommit(b.raw(commit), pool, taskID(t, "MGIT-11.10.10"), nil)
	require.NoError(t, err)

	assert.Equal(t, commit.String(), c.CommitID)
	assert.Equal(t, "MGIT-11.10.10", c.TaskID.String())
	require.Len(t, c.FileDiffs, 1)
	assert.Equal(t, "a.txt", c.FileDiffs[0].Path)
	assert.Equal(t, model.DiffAdded, c.FileDiffs[0].Operation)

	// The orchestrator re-verifies these exact bindings; they must pass.
	require.NoError(t, VerifyBinding(b.raw(commit), c))
	require.NoError(t, VerifyTreeBinding(pool, c, nil))
}

// TestDeriveLandedCommit_AgainstParent_RecordsModify verifies a derived
// commit's diff is computed against the parent file set (modify, not add).
func TestDeriveLandedCommit_AgainstParent_RecordsModify(t *testing.T) {
	b := newBuilder(t)
	when := time.Date(2026, 6, 21, 10, 0, 0, 0, time.UTC)
	oldBlob := b.writeBlob("v1")
	newBlob := b.writeBlob("v2")
	tree := b.writeTree(object.TreeEntry{Name: "a.txt", Mode: filemode.Regular, Hash: newBlob})
	commit := b.writeCommit("feat: edit a.txt", "agent-7", tree, plumbing.ZeroHash, when)
	pool := []Object{{Type: ObjBlob, Data: b.raw(newBlob)}, {Type: ObjTree, Data: b.raw(tree)}, {Type: ObjCommit, Data: b.raw(commit)}}

	parentFiles := map[string]string{"a.txt": oldBlob.String()}
	c, err := DeriveLandedCommit(b.raw(commit), pool, taskID(t, "MGIT-1"), parentFiles)
	require.NoError(t, err)
	require.Len(t, c.FileDiffs, 1)
	assert.Equal(t, model.DiffModified, c.FileDiffs[0].Operation)
	require.NoError(t, VerifyTreeBinding(pool, c, parentFiles))
}

// TestDeriveLandedCommit_TreeMissingFromPool_Error verifies derivation fails
// closed when the commit's tree object is absent from the land pool (the
// landed file set cannot be resolved without it).
func TestDeriveLandedCommit_TreeMissingFromPool_Error(t *testing.T) {
	b := newBuilder(t)
	tree := b.writeTree(object.TreeEntry{Name: "a.txt", Mode: filemode.Regular, Hash: b.writeBlob("x")})
	commit := b.writeCommit("feat: x", "a", tree, plumbing.ZeroHash, time.Unix(0, 0).UTC())
	// Pool omits the tree object, so the landed tree is unresolvable.
	pool := []Object{{Type: ObjCommit, Data: b.raw(commit)}}
	_, err := DeriveLandedCommit(b.raw(commit), pool, taskID(t, "MGIT-1"), nil)
	assert.ErrorIs(t, err, model.ErrLandVerificationFailed)
}

// TestCommitChain_OrdersParentFirst_SkipsLanded verifies a two-commit chain
// is ordered oldest-first and base history is excluded via skip.
func TestCommitChain_OrdersParentFirst_SkipsLanded(t *testing.T) {
	b := newBuilder(t)
	when := time.Date(2026, 6, 21, 11, 0, 0, 0, time.UTC)
	tree := b.writeTree(object.TreeEntry{Name: "a.txt", Mode: filemode.Regular, Hash: b.writeBlob("x")})
	base := b.writeCommit("base", "a", tree, plumbing.ZeroHash, when) // already on host
	c1 := b.writeCommit("c1", "a", tree, base, when.Add(time.Minute)) // new
	c2 := b.writeCommit("c2", "a", tree, c1, when.Add(2*time.Minute)) // new, child of c1
	pool := []Object{
		{Type: ObjCommit, Data: b.raw(c2)},
		{Type: ObjCommit, Data: b.raw(base)},
		{Type: ObjCommit, Data: b.raw(c1)},
	}

	landed := map[string]bool{base.String(): true}
	chain, err := CommitChain(pool, func(id string) bool { return landed[id] })
	require.NoError(t, err)
	require.Len(t, chain, 2)
	assert.Equal(t, c1.String(), chain[0].ID, "parent comes first")
	assert.Equal(t, c2.String(), chain[1].ID)
	assert.Equal(t, c1.String(), chain[1].ParentID)
}

func TestCommitChain_NothingNew_Empty(t *testing.T) {
	b := newBuilder(t)
	tree := b.writeTree(object.TreeEntry{Name: "a.txt", Mode: filemode.Regular, Hash: b.writeBlob("x")})
	c := b.writeCommit("c", "a", tree, plumbing.ZeroHash, time.Unix(0, 0).UTC())
	pool := []Object{{Type: ObjCommit, Data: b.raw(c)}}
	chain, err := CommitChain(pool, func(string) bool { return true })
	require.NoError(t, err)
	assert.Empty(t, chain)
}

func TestCommitChain_ForkedBatch_Rejected(t *testing.T) {
	b := newBuilder(t)
	when := time.Unix(0, 0).UTC()
	tree := b.writeTree(object.TreeEntry{Name: "a.txt", Mode: filemode.Regular, Hash: b.writeBlob("x")})
	// Two independent roots (both parent the host tip / empty): not a chain.
	c1 := b.writeCommit("c1", "a", tree, plumbing.ZeroHash, when)
	c2 := b.writeCommit("c2", "a", tree, plumbing.ZeroHash, when.Add(time.Minute))
	pool := []Object{{Type: ObjCommit, Data: b.raw(c1)}, {Type: ObjCommit, Data: b.raw(c2)}}
	_, err := CommitChain(pool, func(string) bool { return false })
	assert.ErrorIs(t, err, model.ErrLandVerificationFailed)
}

func TestCommitChain_DuplicateCommit_Rejected(t *testing.T) {
	b := newBuilder(t)
	tree := b.writeTree(object.TreeEntry{Name: "a.txt", Mode: filemode.Regular, Hash: b.writeBlob("x")})
	c := b.writeCommit("c", "a", tree, plumbing.ZeroHash, time.Unix(0, 0).UTC())
	pool := []Object{{Type: ObjCommit, Data: b.raw(c)}, {Type: ObjCommit, Data: b.raw(c)}}
	_, err := CommitChain(pool, func(string) bool { return false })
	assert.ErrorIs(t, err, model.ErrLandVerificationFailed)
}
