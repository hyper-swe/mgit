package land

import (
	"context"
	"io"
	"testing"
	"time"

	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/object"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/hyper-swe/mgit/internal/model"
	gitstore "github.com/hyper-swe/mgit/internal/store/git"
)

// newHostRepo opens a fresh host repository (shared .mgit object store).
func newHostRepo(t *testing.T) *gitstore.Repository {
	t.Helper()
	repo, err := gitstore.Init(t.TempDir(), func() time.Time { return time.Unix(0, 0).UTC() })
	require.NoError(t, err)
	return repo
}

// commitObject builds a real canonical git commit object, returning its
// content bytes and content-addressed hash.
func commitObject(t *testing.T, msg string) (content []byte, hash string) {
	t.Helper()
	sig := object.Signature{Name: "agent-1", Email: "a@x", When: time.Date(2026, 6, 20, 12, 0, 0, 0, time.UTC)}
	gc := &object.Commit{Author: sig, Committer: sig, Message: msg, TreeHash: plumbing.ZeroHash}
	enc := &plumbing.MemoryObject{}
	require.NoError(t, gc.Encode(enc))
	r, err := enc.Reader()
	require.NoError(t, err)
	content, err = io.ReadAll(r)
	require.NoError(t, err)
	return content, enc.Hash().String()
}

// TestStoreImporter_ImportsRetrievableObjects verifies a verified commit
// object imported into the host store is afterwards resolvable there — the
// concrete land object-import that makes land actually persist objects.
func TestStoreImporter_ImportsRetrievableObjects(t *testing.T) {
	repo := newHostRepo(t)
	imp := NewStoreImporter(repo)
	content, hash := commitObject(t, "landed-commit")

	require.NoError(t, imp.ImportObjects(context.Background(), []Object{{Type: ObjCommit, Data: content}}))

	got, err := gitstore.NewCommitStore(repo).GetCommit(context.Background(), hash)
	require.NoError(t, err, "the imported commit is resolvable in the host store")
	assert.Equal(t, "landed-commit", got.Message)
}

// TestStoreImporter_ImportsAllTypes verifies blobs and trees import too
// (the full object pool a land carries), not just commits.
func TestStoreImporter_ImportsAllTypes(t *testing.T) {
	repo := newHostRepo(t)
	imp := NewStoreImporter(repo)
	commit, _ := commitObject(t, "c")

	// A tree object and a blob object (raw canonical content).
	blob := &plumbing.MemoryObject{}
	blob.SetType(plumbing.BlobObject)
	_, _ = blob.Write([]byte("file body"))
	br, _ := blob.Reader()
	blobData, _ := io.ReadAll(br)

	tree := &object.Tree{}
	enc := &plumbing.MemoryObject{}
	require.NoError(t, tree.Encode(enc))
	tr, _ := enc.Reader()
	treeData, _ := io.ReadAll(tr)

	err := imp.ImportObjects(context.Background(), []Object{
		{Type: ObjBlob, Data: blobData},
		{Type: ObjTree, Data: treeData},
		{Type: ObjCommit, Data: commit},
	})
	require.NoError(t, err, "a mixed object pool imports without error")
}

// TestStoreImporter_UnknownType_Rejected verifies a frame tag that is not
// a known git object kind fails closed.
func TestStoreImporter_UnknownType_Rejected(t *testing.T) {
	imp := NewStoreImporter(newHostRepo(t))
	err := imp.ImportObjects(context.Background(), []Object{{Type: 'Z', Data: []byte("x")}})
	require.ErrorIs(t, err, model.ErrLandVerificationFailed)
}

// TestStoreImporter_Idempotent verifies importing the same pool twice is a
// harmless no-op (content-addressed), as Land relies on (FR-17.5).
func TestStoreImporter_Idempotent(t *testing.T) {
	repo := newHostRepo(t)
	imp := NewStoreImporter(repo)
	content, _ := commitObject(t, "again")
	objs := []Object{{Type: ObjCommit, Data: content}}
	require.NoError(t, imp.ImportObjects(context.Background(), objs))
	require.NoError(t, imp.ImportObjects(context.Background(), objs))
}
