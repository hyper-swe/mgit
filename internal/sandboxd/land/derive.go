package land

import (
	"fmt"

	"github.com/go-git/go-git/v5/plumbing"

	"github.com/hyper-swe/mgit/internal/model"
	gitstore "github.com/hyper-swe/mgit/internal/store/git"
)

// PoolCommit is one commit object found in a pulled land pool, paired with
// the exact bytes it will verify and import. ID is the git object id
// (content hash of the bytes, SEC-06) and ParentID its first parent, both
// derived host-side from the object — never guest-asserted. Refs: FR-17.5
type PoolCommit struct {
	ID       string
	ParentID string
	Data     []byte
}

// CommitChain returns the NEW commit objects in a land pool ordered
// parent-first, ready to derive and verify in branch-append order. A
// commit for which skip(id) reports true is already in the host store
// (base history the guest re-streamed because it streams everything
// reachable from its branch head) and is excluded. The remaining commits
// MUST form a single linear chain rooted at a commit whose parent is not
// itself new (its parent is the host branch tip, or empty for an initial
// commit) — mgit micro-commits are linear, so a fork or gap in the new set
// is a schema violation that lands nothing. All ids are host-computed from
// the bytes (SEC-06); the guest asserts no ordering. Refs: FR-17.5, SEC-06
func CommitChain(pool []Object, skip func(id string) bool) ([]PoolCommit, error) {
	newByID, err := newPoolCommits(pool, skip)
	if err != nil {
		return nil, err
	}
	if len(newByID) == 0 {
		return nil, nil
	}
	return orderChain(newByID)
}

// newPoolCommits decodes every commit object in the pool, computes its
// host-side id and parent, and keeps only those not already landed. A
// duplicate id (the same commit served twice) is a schema violation.
func newPoolCommits(pool []Object, skip func(id string) bool) (map[string]PoolCommit, error) {
	byID := make(map[string]PoolCommit)
	for _, obj := range pool {
		if obj.Type != ObjCommit {
			continue
		}
		id := plumbing.ComputeHash(plumbing.CommitObject, obj.Data).String()
		if _, dup := byID[id]; dup {
			return nil, fmt.Errorf("%w: duplicate commit object %s", model.ErrLandVerificationFailed, id)
		}
		if skip(id) {
			continue
		}
		derived, err := gitstore.CommitFromObjectData(obj.Data)
		if err != nil {
			return nil, fmt.Errorf("%w: %w", model.ErrLandVerificationFailed, err)
		}
		byID[id] = PoolCommit{ID: id, ParentID: derived.ParentID, Data: obj.Data}
	}
	return byID, nil
}

// orderChain orders a non-empty set of new commits parent-first, requiring
// exactly one root (its parent is not in the set) and a single unbroken
// chain through every member.
func orderChain(newByID map[string]PoolCommit) ([]PoolCommit, error) {
	childOf := make(map[string]string, len(newByID)) // parent id -> child id
	var roots []PoolCommit
	for _, pc := range newByID {
		if _, parentIsNew := newByID[pc.ParentID]; parentIsNew {
			childOf[pc.ParentID] = pc.ID
		} else {
			roots = append(roots, pc)
		}
	}
	if len(roots) != 1 {
		return nil, fmt.Errorf("%w: land batch is not a single commit chain (%d roots)",
			model.ErrLandVerificationFailed, len(roots))
	}

	ordered := make([]PoolCommit, 0, len(newByID))
	for cur := roots[0]; ; {
		ordered = append(ordered, cur)
		childID, ok := childOf[cur.ID]
		if !ok {
			break
		}
		cur = newByID[childID]
	}
	if len(ordered) != len(newByID) {
		return nil, fmt.Errorf("%w: land batch has a forked or detached commit",
			model.ErrLandVerificationFailed)
	}
	return ordered, nil
}

// DeriveLandedCommit reconstructs a fully-bound model.Commit for one commit
// pulled over the land channel, entirely host-side and from the exact bytes
// (SEC-01/SEC-06): identity and audit metadata come from the object bytes
// (CommitFromObjectData), the FileDiffs are recomputed from the landed tree
// against the parent file set, and content_hash is computed over the
// result. The returned commit therefore passes VerifyBinding and
// VerifyTreeBinding by construction — the orchestrator re-verifies it as the
// single chokepoint, so a derivation bug cannot smuggle anything past
// verification. taskID is the host-anchored task the sandbox is bound to,
// never guest-asserted text. parentFiles is the parent commit's file set
// from the host store (empty for an initial commit). Refs: FR-17.5, FR-17.24, SEC-01, SEC-06
func DeriveLandedCommit(objData []byte, pool []Object, taskID model.TaskID, parentFiles map[string]string) (*model.Commit, error) {
	c, err := gitstore.CommitFromObjectData(objData)
	if err != nil {
		return nil, fmt.Errorf("%w: %w", model.ErrLandVerificationFailed, err)
	}
	storer, err := poolStorer(pool)
	if err != nil {
		return nil, err
	}
	landedFiles, err := landedFileSet(storer, c.TreeHash)
	if err != nil {
		return nil, err
	}
	c.TaskID = taskID
	c.FileDiffs = diffFileSets(parentFiles, landedFiles)
	c.ContentHash = c.ComputeContentHash()
	return c, nil
}
