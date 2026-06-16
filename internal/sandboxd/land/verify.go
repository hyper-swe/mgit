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

	"github.com/go-git/go-git/v5/plumbing"

	"github.com/hyper-swe/mgit/internal/model"
)

// Importer writes a verified commit into the host store. It receives the
// SAME objectData buffer and commit that were just verified, so there is
// no second read between verification and import — a guest cannot serve
// different bytes on a re-fetch (SEC-06). Refs: FR-17.24
type Importer interface {
	Import(objectData []byte, c *model.Commit) error
}

// VerifyCommit recomputes BOTH ADR-002 hashes from a single in-memory
// landed commit and imports that same commit only if both match what it
// claims. The git SHA-1 object id is recomputed from the canonical
// commit object bytes (go-git, so it equals what git itself stores); the
// SHA-256 content_hash from the commit's fields (the single authoritative
// model.Commit.ComputeContentHash). Because it hashes exactly the bytes
// and fields it imports, there is no verify-then-refetch window: a guest
// serving different bytes on a second read cannot pass (SEC-06,
// FR-17.24). Any mismatch returns ErrLandVerificationFailed and imports
// nothing.
func VerifyCommit(objectData []byte, c *model.Commit, imp Importer) error {
	if c == nil {
		return fmt.Errorf("%w: nil commit", model.ErrLandVerificationFailed)
	}
	gotGitID := plumbing.ComputeHash(plumbing.CommitObject, objectData).String()
	if gotGitID != c.CommitID {
		return fmt.Errorf("%w: git object id %s does not match claimed %s",
			model.ErrLandVerificationFailed, gotGitID, c.CommitID)
	}
	if gotContent := c.ComputeContentHash(); gotContent != c.ContentHash {
		return fmt.Errorf("%w: recomputed content_hash does not match claimed",
			model.ErrLandVerificationFailed)
	}
	return imp.Import(objectData, c)
}
