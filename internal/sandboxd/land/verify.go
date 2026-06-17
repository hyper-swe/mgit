// Package land implements the sandbox land boundary: pulling commit
// objects from a guest over vsock and importing them into the host store
// only after host-side re-verification. The guest is the hostile party,
// so the host trusts nothing it asserts — it recomputes both ADR-002
// hashes from the exact bytes it will import (hash-on-write, SEC-06) and
// rejects any commit whose bytes or fields do not hash to what they
// claim. Host-side, pure Go. Refs: FR-17.5, FR-17.24, SEC-06, ADR-002,
// MGIT-11.8.3
package land

import (
	"fmt"

	"github.com/hyper-swe/mgit/internal/model"
	gitstore "github.com/hyper-swe/mgit/internal/store/git"
)

// Importer writes a verified commit into the host store. It receives the
// SAME objectData buffer and commit that were just verified, so there is
// no second read between verification and import — a guest cannot serve
// different bytes on a re-fetch (SEC-06). Refs: FR-17.24
type Importer interface {
	Import(objectData []byte, c *model.Commit) error
}

// VerifyCommit verifies a landed commit against the single in-memory
// object buffer and imports that same commit only if every check passes.
// It performs three bindings, all derived from the exact bytes it will
// import (hash-on-write — no verify-then-refetch window, SEC-06/FR-17.24):
//
//  1. Identity: the canonical commit object decoded from objectData must
//     hash to the claimed CommitID (the git SHA-1 object id).
//  2. Metadata (SEC-06, F2 closure): the commit's audit-bearing fields —
//     message, tree, parent, author/agent, time — must equal those
//     decoded FROM the object bytes. A guest cannot present honest bytes
//     with lying metadata: the fields the audit trail records are bound
//     to the content-addressed object, not taken on the guest's word.
//  3. Integrity: the ADR-002 SHA-256 content_hash (the single
//     authoritative model.Commit.ComputeContentHash) must match.
//
// Any mismatch returns ErrLandVerificationFailed and imports nothing.
// FileDiffs are tree-derived (not in the commit object) and are bound
// when the tree objects land and the diff is recomputed (MGIT-11.9).
func VerifyCommit(objectData []byte, c *model.Commit, imp Importer) error {
	if c == nil {
		return fmt.Errorf("%w: nil commit", model.ErrLandVerificationFailed)
	}
	// Derive the canonical commit from the exact bytes (single-source
	// git→model mapping). This both recomputes the SHA-1 id and yields
	// the object's true metadata.
	derived, err := gitstore.CommitFromObjectData(objectData)
	if err != nil {
		return fmt.Errorf("%w: %w", model.ErrLandVerificationFailed, err)
	}
	if derived.CommitID != c.CommitID {
		return fmt.Errorf("%w: git object id %s does not match claimed %s",
			model.ErrLandVerificationFailed, derived.CommitID, c.CommitID)
	}
	if mismatch := metadataMismatch(derived, c); mismatch != "" {
		return fmt.Errorf("%w: commit metadata does not match the object bytes: %s",
			model.ErrLandVerificationFailed, mismatch)
	}
	if c.ComputeContentHash() != c.ContentHash {
		return fmt.Errorf("%w: recomputed content_hash does not match claimed",
			model.ErrLandVerificationFailed)
	}
	return imp.Import(objectData, c)
}

// metadataMismatch returns the first audit field of want that diverges
// from the object-derived commit, or "" if all bind. Only fields carried
// by a git commit object are checked here (FileDiffs are tree-derived).
func metadataMismatch(derived, want *model.Commit) string {
	switch {
	case derived.Message != want.Message:
		return "message"
	case derived.TreeHash != want.TreeHash:
		return "tree_hash"
	case derived.ParentID != want.ParentID:
		return "parent_id"
	case derived.AgentID != want.AgentID:
		return "agent_id"
	case !derived.CreatedAt.Equal(want.CreatedAt):
		return "created_at"
	}
	return ""
}
