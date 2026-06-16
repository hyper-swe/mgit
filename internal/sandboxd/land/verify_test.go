// Package land tests verify the TOCTOU-safe hash-on-write dual-hash
// verification at the sandbox land boundary (SEC-06, FR-17.24, ADR-002).
// Refs: MGIT-11.8.3
package land

import (
	"testing"
	"time"

	"github.com/go-git/go-git/v5/plumbing"
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

// validLanded builds a self-consistent landed commit: object bytes whose
// git SHA-1 matches CommitID, and fields whose SHA-256 matches ContentHash.
func validLanded(t *testing.T) ([]byte, *model.Commit) {
	t.Helper()
	taskID, err := model.ParseTaskID("MGIT-11.8.3")
	require.NoError(t, err)
	objectData := []byte("tree 0000\nauthor a <a@x> 0 +0000\ncommitter a <a@x> 0 +0000\n\nfeat: land me\n")
	c := &model.Commit{
		TaskID:    taskID,
		Message:   "feat: land me",
		ParentID:  "",
		CreatedAt: time.Date(2026, 6, 16, 12, 0, 0, 0, time.UTC),
	}
	c.CommitID = plumbing.ComputeHash(plumbing.CommitObject, objectData).String()
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
	t.Run("field_tampered_breaks_content_hash", func(t *testing.T) {
		objectData, c := validLanded(t)
		c.Message += " (tampered after hashing)" // message no longer matches ContentHash
		imp := &captureImporter{}
		assert.ErrorIs(t, VerifyCommit(objectData, c, imp), model.ErrLandVerificationFailed)
		assert.False(t, imp.called)
	})
}

// TestVerifyCommit_NilCommit covers the defensive guard.
func TestVerifyCommit_NilCommit(t *testing.T) {
	assert.ErrorIs(t, VerifyCommit([]byte("x"), nil, &captureImporter{}), model.ErrLandVerificationFailed)
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
