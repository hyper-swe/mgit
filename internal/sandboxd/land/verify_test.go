// Package land tests verify the TOCTOU-safe hash-on-write dual-hash
// verification at the sandbox land boundary (SEC-06, FR-17.24, ADR-002).
// Refs: MGIT-11.8.3
package land

import (
	"io"
	"testing"
	"time"

	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/object"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/hyper-swe/mgit/internal/model"
)

// captureImporter records what it was asked to import.
type captureImporter struct {
	called    bool
	gotData   []byte
	gotCommit *model.Commit
}

func (c *captureImporter) Import(objectData []byte, commit *model.Commit) error {
	c.called = true
	c.gotData = objectData
	c.gotCommit = commit
	return nil
}

// validLanded builds a self-consistent landed commit: a REAL canonical
// git commit object, and a model.Commit whose identity-bearing fields are
// derived from that object (so the F2 metadata binding holds) and whose
// SHA-256 content_hash matches its own fields.
func validLanded(t *testing.T) ([]byte, *model.Commit) {
	t.Helper()
	when := time.Date(2026, 6, 16, 12, 0, 0, 0, time.UTC)
	sig := object.Signature{Name: "agent-1", Email: "a@x", When: when}
	gc := &object.Commit{Author: sig, Committer: sig, Message: "feat: land me", TreeHash: plumbing.ZeroHash}

	enc := &plumbing.MemoryObject{}
	require.NoError(t, gc.Encode(enc))
	r, err := enc.Reader()
	require.NoError(t, err)
	objectData, err := io.ReadAll(r)
	require.NoError(t, err)

	c := &model.Commit{
		CommitID:  enc.Hash().String(),
		Message:   "feat: land me",
		CreatedAt: when,
		ParentID:  "",
		TreeHash:  plumbing.ZeroHash.String(),
		AgentID:   "agent-1",
	}
	c.ContentHash = c.ComputeContentHash()
	return objectData, c
}

// TestLand_DualHashRecomputed verifies a self-consistent commit verifies
// (both SHA-1 and SHA-256 recomputed and matched) and is imported.
// Refs: FR-17.24, ADR-002
func TestLand_DualHashRecomputed(t *testing.T) {
	objectData, c := validLanded(t)
	imp := &captureImporter{}
	require.NoError(t, VerifyCommit(objectData, c, imp))
	assert.True(t, imp.called, "a verified commit is imported")
}

// TestLand_HashesImportedBuffer_NotRefetch verifies the importer receives
// the exact buffer that was hashed — verification and import operate on
// the same in-memory bytes, not a second fetch. Refs: SEC-06, FR-17.24
func TestLand_HashesImportedBuffer_NotRefetch(t *testing.T) {
	objectData, c := validLanded(t)
	imp := &captureImporter{}
	require.NoError(t, VerifyCommit(objectData, c, imp))

	// The imported bytes are byte-identical to the verified buffer, and
	// re-hashing them yields the same id that verification checked.
	assert.Equal(t, objectData, imp.gotData)
	assert.Equal(t, c.CommitID, plumbing.ComputeHash(plumbing.CommitObject, imp.gotData).String(),
		"the imported bytes hash to the verified id — no divergent re-fetch")
}

// TestLand_TOCTOUByteSwap_Fails verifies a guest that claims one commit
// id but serves different object bytes cannot pass: the host hashes the
// bytes it would import, so the swap is caught. Refs: SEC-06, FR-17.24
func TestLand_TOCTOUByteSwap_Fails(t *testing.T) {
	objectData, c := validLanded(t)
	// The guest swaps the bytes after computing its claimed id.
	swapped := append([]byte("tampered "), objectData...)
	imp := &captureImporter{}

	err := VerifyCommit(swapped, c, imp) // c.CommitID still claims the original
	require.ErrorIs(t, err, model.ErrLandVerificationFailed)
	assert.False(t, imp.called, "nothing is imported when the served bytes do not match the claim")
}

// TestLand_HashMismatch_VerificationFailed verifies each hash is checked:
// a tampered content_hash and a tampered commit id both fail, and import
// is skipped. Refs: FR-17.24
func TestLand_HashMismatch_VerificationFailed(t *testing.T) {
	t.Run("content_hash_tampered", func(t *testing.T) {
		objectData, c := validLanded(t)
		c.ContentHash = "0000000000000000000000000000000000000000000000000000000000000000"
		imp := &captureImporter{}
		assert.ErrorIs(t, VerifyCommit(objectData, c, imp), model.ErrLandVerificationFailed)
		assert.False(t, imp.called)
	})
	t.Run("commit_id_tampered", func(t *testing.T) {
		objectData, c := validLanded(t)
		c.CommitID = "0000000000000000000000000000000000000000"
		imp := &captureImporter{}
		assert.ErrorIs(t, VerifyCommit(objectData, c, imp), model.ErrLandVerificationFailed)
		assert.False(t, imp.called)
	})
	t.Run("message_tampered_breaks_object_binding", func(t *testing.T) {
		objectData, c := validLanded(t)
		c.Message += " (tampered after hashing)"
		c.ContentHash = c.ComputeContentHash() // keep content_hash self-consistent
		imp := &captureImporter{}
		// The message no longer matches the object bytes → metadata binding fails.
		assert.ErrorIs(t, VerifyCommit(objectData, c, imp), model.ErrLandVerificationFailed)
		assert.False(t, imp.called)
	})
}

// TestLand_MetadataDecoupledFromObject_Rejected verifies the F2 binding:
// a guest cannot present HONEST object bytes alongside lying audit
// metadata (a self-consistent content_hash over the lie). The host
// derives the true fields from the bytes and rejects the mismatch.
// Refs: SEC-06, FR-17.24
func TestLand_MetadataDecoupledFromObject_Rejected(t *testing.T) {
	objectData, _ := validLanded(t) // honest bytes (message "feat: land me", agent-1)
	// The guest claims different audit metadata, with a content_hash that
	// is internally consistent with the lie (so check #3 alone would pass).
	liar := &model.Commit{
		CommitID:  mustHash(t, objectData), // honest id (passes the SHA-1 check)
		Message:   "chore: totally benign",
		CreatedAt: time.Date(2026, 6, 16, 12, 0, 0, 0, time.UTC),
		TreeHash:  plumbing.ZeroHash.String(),
		AgentID:   "victim-agent", // attributing the commit to someone else
	}
	liar.ContentHash = liar.ComputeContentHash()

	imp := &captureImporter{}
	err := VerifyCommit(objectData, liar, imp)
	require.ErrorIs(t, err, model.ErrLandVerificationFailed)
	assert.Contains(t, err.Error(), "metadata does not match the object bytes")
	assert.False(t, imp.called, "lying metadata over honest bytes must not land")
}

// mustHash returns the git commit-object id of objectData.
func mustHash(t *testing.T, objectData []byte) string {
	t.Helper()
	return plumbing.ComputeHash(plumbing.CommitObject, objectData).String()
}

// TestLand_EachMetadataField_Bound verifies every audit field carried by
// the commit object is bound to the bytes: tampering any one is rejected.
// Refs: SEC-06, FR-17.24
func TestLand_EachMetadataField_Bound(t *testing.T) {
	for _, tc := range []struct {
		field  string
		tamper func(*model.Commit)
	}{
		{"message", func(c *model.Commit) { c.Message = "different" }},
		{"tree_hash", func(c *model.Commit) { c.TreeHash = "ffffffffffffffffffffffffffffffffffffffff" }},
		{"parent_id", func(c *model.Commit) { c.ParentID = "ffffffffffffffffffffffffffffffffffffffff" }},
		{"agent_id", func(c *model.Commit) { c.AgentID = "impostor" }},
		{"created_at", func(c *model.Commit) { c.CreatedAt = c.CreatedAt.Add(time.Hour) }},
	} {
		t.Run(tc.field, func(t *testing.T) {
			objectData, c := validLanded(t)
			tc.tamper(c)
			c.ContentHash = c.ComputeContentHash() // keep content_hash self-consistent
			imp := &captureImporter{}
			err := VerifyCommit(objectData, c, imp)
			require.ErrorIs(t, err, model.ErrLandVerificationFailed)
			assert.Contains(t, err.Error(), tc.field, "the offending field is named")
			assert.False(t, imp.called)
		})
	}
}

// TestVerifyCommit_NilCommit covers the defensive guard.
func TestVerifyCommit_NilCommit(t *testing.T) {
	assert.ErrorIs(t, VerifyCommit([]byte("x"), nil, &captureImporter{}), model.ErrLandVerificationFailed)
}

// TestVerifyCommit_UndecodableObject_Rejected verifies bytes that are not
// a valid git commit object fail closed at decode. Refs: FR-17.24
func TestVerifyCommit_UndecodableObject_Rejected(t *testing.T) {
	imp := &captureImporter{}
	err := VerifyCommit([]byte("this is not a git commit object"),
		&model.Commit{CommitID: "1111111111111111111111111111111111111111"}, imp)
	require.ErrorIs(t, err, model.ErrLandVerificationFailed)
	assert.False(t, imp.called)
}

// TestVerifyCommit_ImporterErrorSurfaces verifies an import failure on a
// verified commit propagates (land is not silently successful).
func TestVerifyCommit_ImporterErrorSurfaces(t *testing.T) {
	objectData, c := validLanded(t)
	err := VerifyCommit(objectData, c, importerFunc(func([]byte, *model.Commit) error {
		return assert.AnError
	}))
	assert.ErrorIs(t, err, assert.AnError)
}

// importerFunc adapts a func to Importer.
type importerFunc func(objectData []byte, c *model.Commit) error

func (f importerFunc) Import(objectData []byte, c *model.Commit) error { return f(objectData, c) }
