package git

import (
	"context"
	"errors"
	"fmt"

	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/object"

	"github.com/hyper-swe/mgit/internal/model"
)

// MergeStore exposes the low-level merge primitives needed by the
// MergeService: ancestor checks, fast-forward ref updates, two-parent
// commit creation, and conflict path detection.
// Refs: FR-8.4, MGIT-4.2.10
type MergeStore struct {
	repo *Repository
}

// NewMergeStore creates a MergeStore backed by the given Repository.
func NewMergeStore(repo *Repository) *MergeStore {
	return &MergeStore{repo: repo}
}

// MergeBase returns the lowest common ancestor commit of two commits,
// or an empty string if no common ancestor exists.
// Refs: FR-8.4
func (m *MergeStore) MergeBase(_ context.Context, leftHash, rightHash string) (string, error) {
	left, err := m.commitObject(leftHash)
	if err != nil {
		return "", err
	}
	right, err := m.commitObject(rightHash)
	if err != nil {
		return "", err
	}
	bases, err := left.MergeBase(right)
	if err != nil {
		return "", fmt.Errorf("merge base: %w", err)
	}
	if len(bases) == 0 {
		return "", nil
	}
	return bases[0].Hash.String(), nil
}

// IsAncestor reports whether ancestor is reachable from descendant by
// walking parent links. This is the basis for fast-forward decisions.
// Refs: FR-8.4
func (m *MergeStore) IsAncestor(_ context.Context, ancestor, descendant string) (bool, error) {
	a, err := m.commitObject(ancestor)
	if err != nil {
		return false, err
	}
	d, err := m.commitObject(descendant)
	if err != nil {
		return false, err
	}
	ok, err := a.IsAncestor(d)
	if err != nil {
		return false, fmt.Errorf("ancestor check: %w", err)
	}
	return ok, nil
}

// FastForward advances branchName to point at targetHash without creating a new
// commit, and MATERIALIZES targetHash's tree onto the working tree on disk so
// the working copy reflects the advanced tip rather than being left stale. It
// refuses to overwrite uncommitted tracked changes (ErrRollbackConflict) and
// materializes BEFORE moving the ref, so a failure leaves the branch unmoved.
// The ref move is CAS-guarded against the value just read so a concurrent move
// fails loudly rather than clobbering the branch tip. Returns ErrBranchNotFound
// if branchName does not exist. Refs: FR-8.4, MGIT-15
func (m *MergeStore) FastForward(ctx context.Context, branchName, targetHash string) error {
	refName := plumbing.NewBranchReferenceName(branchName)
	cur, err := m.repo.repo.Storer.Reference(refName)
	if err != nil {
		return fmt.Errorf("%w: %s", model.ErrBranchNotFound, branchName)
	}
	// Materialize the advanced tip BEFORE moving the ref, with a deletion pass
	// keyed off the current (pre-move) HEAD tree, so the working tree reflects
	// the fast-forward (MGIT-15).
	currentHead, err := m.repo.headFiles()
	if err != nil {
		return fmt.Errorf("fast-forward: %w", err)
	}
	if err := m.materializeMerge(ctx, plumbing.NewHash(targetHash), currentHead); err != nil {
		return fmt.Errorf("fast-forward: %w", err)
	}
	// CAS against the value we just read: a concurrent move fails loudly rather
	// than clobbering the branch tip.
	if err := advanceBranchRefCAS(m.repo.repo.Storer, refName, plumbing.NewHash(targetHash), cur.Hash()); err != nil {
		return fmt.Errorf("fast-forward: %w", err)
	}
	return nil
}

// CreateMergeCommit creates a two-parent merge commit on the current HEAD
// (HEAD's previous commit + the source commit), built entirely via plumbing —
// no go-git worktree. The merge commit's tree REFLECTS THE MERGE: a
// non-conflicting union of both sides' changes against their merge base (HEAD's
// version where only HEAD changed a path, the source's version where only the
// source changed it, the base's otherwise). The caller (MergeService) detects
// any path changed on both sides via ConflictingPaths before calling this, so
// the union is well-defined. The merged tree is materialized onto disk so the
// working tree reflects the merge; the merge refuses to clobber uncommitted
// tracked changes (ErrRollbackConflict). Returns the merge commit's hash.
// Refs: FR-8.4, MGIT-14.4, MGIT-15
func (m *MergeStore) CreateMergeCommit(ctx context.Context, message, sourceHash string) (string, error) {
	goRepo := m.repo.repo

	headRef, err := m.repo.currentRef()
	if err != nil {
		return "", fmt.Errorf("merge commit: resolve HEAD: %w", err)
	}
	headHash := headRef.Hash()

	// Capture the PRE-merge HEAD tree NOW, for the deletion pass during
	// materialization. Reading it after the ref moves would resolve to the merge
	// commit's own tree, so files the merge removed would never be deleted.
	currentHead, err := m.repo.headFiles()
	if err != nil {
		return "", fmt.Errorf("merge commit: %w", err)
	}

	treeHash, err := m.mergedTree(headHash.String(), sourceHash)
	if err != nil {
		return "", fmt.Errorf("merge commit: %w", err)
	}

	commitHash, err := writeCommit(goRepo.Storer, commitParams{
		tree:    treeHash,
		parents: []plumbing.Hash{headHash, plumbing.NewHash(sourceHash)},
		message: message,
		authorAt: object.Signature{
			Name:  "mgit-merge",
			Email: "mgit-merge@mgit",
			When:  m.repo.Now(),
		},
	})
	if err != nil {
		return "", fmt.Errorf("merge commit: create: %w", err)
	}

	// Materialize the merged tree onto disk BEFORE moving the ref (so a failure
	// leaves the branch unmoved), refusing to clobber uncommitted tracked work.
	if err := m.materializeMerge(ctx, commitHash, currentHead); err != nil {
		return "", fmt.Errorf("merge commit: %w", err)
	}

	// Advance the current branch ref to the new merge commit with a CAS against
	// the HEAD we read, so a concurrent move fails loudly instead of orphaning
	// the merge commit.
	if err := advanceBranchRefCAS(goRepo.Storer, headRef.Name(), commitHash, headHash); err != nil {
		return "", fmt.Errorf("merge commit: update ref: %w", err)
	}
	return commitHash.String(), nil
}

// materializeMerge writes target's tree onto the working tree (with a deletion
// pass keyed off preHead) after refusing to clobber uncommitted changes to
// tracked files — a merge must not silently destroy a dirty working tree, the
// same guard Checkout applies. Returns ErrRollbackConflict when the tree is
// dirty. Refs: FR-8.4, MGIT-15
func (m *MergeStore) materializeMerge(ctx context.Context, target plumbing.Hash, preHead map[string]blobEntry) error {
	ws := NewWorktreeStore(m.repo)
	dirty, err := ws.dirtyTrackedPaths(ctx)
	if err != nil {
		return fmt.Errorf("check working tree: %w", err)
	}
	if len(dirty) > 0 {
		return fmt.Errorf("%w: merge would overwrite uncommitted changes: %v", model.ErrRollbackConflict, dirty)
	}
	return ws.materializeCommit(target, preHead)
}

// mergedTree builds and stores the tree for a two-parent merge of headHash and
// sourceHash and returns its hash. The result is the base tree with each side's
// own net delta layered on: a path changed only by HEAD takes HEAD's entry, a
// path changed only by the source takes the source's entry, and an unchanged
// path keeps the base's entry. Modes (executable/symlink) are carried through
// via blobEntry, never hardcoded. Paths changed on BOTH sides are the caller's
// responsibility to reject as conflicts beforehand; if any reach here, the
// source's entry wins (last delta applied), which is safe but not relied upon.
// Refs: MGIT-15, MGIT-14.7 (#3)
func (m *MergeStore) mergedTree(headHash, sourceHash string) (plumbing.Hash, error) {
	baseHash, err := m.MergeBase(context.Background(), headHash, sourceHash)
	if err != nil {
		return plumbing.ZeroHash, fmt.Errorf("merge base: %w", err)
	}
	var baseFiles map[string]blobEntry
	if baseHash == "" {
		baseFiles = make(map[string]blobEntry)
	} else if baseFiles, err = m.commitFiles(baseHash); err != nil {
		return plumbing.ZeroHash, err
	}

	headDelta, err := m.netDelta(baseHash, headHash)
	if err != nil {
		return plumbing.ZeroHash, err
	}
	sourceDelta, err := m.netDelta(baseHash, sourceHash)
	if err != nil {
		return plumbing.ZeroHash, err
	}

	merged := make(map[string]blobEntry, len(baseFiles))
	for path, e := range baseFiles {
		merged[path] = e
	}
	applyMergeDelta(merged, headDelta)
	applyMergeDelta(merged, sourceDelta)

	treeHash, err := writeNestedTree(m.repo.repo.Storer, merged)
	if err != nil {
		return plumbing.ZeroHash, fmt.Errorf("write merged tree: %w", err)
	}
	return treeHash, nil
}

// netDelta returns one side's net change from base to target as a map of path →
// *blobEntry: a non-nil entry for a path the side added or modified, and nil for
// a path the side deleted. A zero baseHash means base is the empty tree (no
// common ancestor / root). Refs: MGIT-15
func (m *MergeStore) netDelta(baseHash, targetHash string) (map[string]*blobEntry, error) {
	var base map[string]blobEntry
	if baseHash == "" {
		base = make(map[string]blobEntry)
	} else {
		var err error
		if base, err = m.commitFiles(baseHash); err != nil {
			return nil, err
		}
	}
	target, err := m.commitFiles(targetHash)
	if err != nil {
		return nil, err
	}
	delta := make(map[string]*blobEntry)
	applyTreeDelta(delta, base, target)
	return delta, nil
}

// applyMergeDelta applies one side's net delta onto the accumulating merged file
// set: a non-nil entry sets/replaces the path, a nil entry deletes it.
// Refs: MGIT-15
func applyMergeDelta(merged map[string]blobEntry, delta map[string]*blobEntry) {
	for path, e := range delta {
		if e == nil {
			delete(merged, path)
			continue
		}
		merged[path] = *e
	}
}

// commitFiles returns the flattened file set (path → blob+mode) of a commit's
// tree, identified by hex hash. Refs: MGIT-15
func (m *MergeStore) commitFiles(hash string) (map[string]blobEntry, error) {
	obj, err := m.commitObject(hash)
	if err != nil {
		return nil, err
	}
	tree, err := obj.Tree()
	if err != nil {
		return nil, fmt.Errorf("read tree for %s: %w", hash, err)
	}
	return flattenTree(tree)
}

// ConflictingPaths returns the list of paths that were modified on both
// sides of a merge between baseHash and the two branch tips. A path is in
// conflict if both branches changed it relative to the base AND the two
// resulting blob hashes differ.
// Refs: FR-8.4
func (m *MergeStore) ConflictingPaths(_ context.Context, baseHash, leftHash, rightHash string) ([]string, error) {
	base, err := m.commitObject(baseHash)
	if err != nil {
		return nil, err
	}
	left, err := m.commitObject(leftHash)
	if err != nil {
		return nil, err
	}
	right, err := m.commitObject(rightHash)
	if err != nil {
		return nil, err
	}

	leftChanges, err := changedPaths(base, left)
	if err != nil {
		return nil, fmt.Errorf("merge conflict scan (left): %w", err)
	}
	rightChanges, err := changedPaths(base, right)
	if err != nil {
		return nil, fmt.Errorf("merge conflict scan (right): %w", err)
	}

	var conflicts []string
	for path, leftHashStr := range leftChanges {
		if rightHashStr, ok := rightChanges[path]; ok && rightHashStr != leftHashStr {
			conflicts = append(conflicts, path)
		}
	}
	return conflicts, nil
}

// changedPaths returns a map of path → blob-hash for files that differ
// between base and target. Files removed in target map to "".
func changedPaths(base, target *object.Commit) (map[string]string, error) {
	baseTree, err := base.Tree()
	if err != nil {
		return nil, fmt.Errorf("base tree: %w", err)
	}
	targetTree, err := target.Tree()
	if err != nil {
		return nil, fmt.Errorf("target tree: %w", err)
	}

	changes, err := baseTree.Diff(targetTree)
	if err != nil {
		return nil, fmt.Errorf("tree diff: %w", err)
	}

	out := make(map[string]string, len(changes))
	for _, ch := range changes {
		path := ch.To.Name
		if path == "" {
			path = ch.From.Name
		}
		hash := ""
		if !ch.To.TreeEntry.Hash.IsZero() {
			hash = ch.To.TreeEntry.Hash.String()
		}
		out[path] = hash
	}
	return out, nil
}

// commitObject loads a go-git commit object by hex hash, returning
// model.ErrCommitNotFound if absent.
func (m *MergeStore) commitObject(hash string) (*object.Commit, error) {
	c, err := m.repo.repo.CommitObject(plumbing.NewHash(hash))
	if err != nil {
		if errors.Is(err, plumbing.ErrObjectNotFound) {
			return nil, fmt.Errorf("%w: %s", model.ErrCommitNotFound, hash)
		}
		return nil, fmt.Errorf("%w: %s", model.ErrCommitNotFound, hash)
	}
	return c, nil
}
