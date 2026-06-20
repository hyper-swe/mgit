package git

import (
	"context"
	"io"
	"testing"
	"time"

	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/object"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// commitObjectContent builds a real canonical git commit object and
// returns its content bytes plus its content-addressed hash.
func commitObjectContent(t *testing.T, msg string) (content []byte, hash string) {
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

// TestWriteRawObject_BlobContentAddressed verifies a blob's raw content is
// stored under its content-addressed hash and the write is idempotent.
func TestWriteRawObject_BlobContentAddressed(t *testing.T) {
	r := initTestRepo(t)
	content := []byte("hello blob content")
	want := plumbing.ComputeHash(plumbing.BlobObject, content).String()

	h, err := r.WriteRawObject(plumbing.BlobObject, content)
	require.NoError(t, err)
	assert.Equal(t, want, h, "object is stored under its content hash")

	h2, err := r.WriteRawObject(plumbing.BlobObject, content)
	require.NoError(t, err)
	assert.Equal(t, h, h2, "re-writing the same bytes is idempotent (content-addressed)")
}

// TestWriteRawObject_CommitRetrievable verifies a raw commit object written
// via WriteRawObject is afterwards resolvable as a commit in the store.
func TestWriteRawObject_CommitRetrievable(t *testing.T) {
	r := initTestRepo(t)
	content, hash := commitObjectContent(t, "raw-import-commit")

	h, err := r.WriteRawObject(plumbing.CommitObject, content)
	require.NoError(t, err)
	assert.Equal(t, hash, h)

	got, err := NewCommitStore(r).GetCommit(context.Background(), h)
	require.NoError(t, err)
	assert.Equal(t, "raw-import-commit", got.Message, "the imported commit is retrievable from the store")
}
