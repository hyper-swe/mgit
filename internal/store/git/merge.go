package git

import (
	"context"
	"errors"
	"fmt"

	gogit "github.com/go-git/go-git/v5"
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

// FastForward advances branchName to point at targetHash without creating
// a new commit. Returns ErrBranchNotFound if branchName does not exist.
// Refs: FR-8.4
func (m *MergeStore) FastForward(_ context.Context, branchName, targetHash string) error {
	refName := plumbing.NewBranchReferenceName(branchName)
	if _, err := m.repo.repo.Storer.Reference(refName); err != nil {
		return fmt.Errorf("%w: %s", model.ErrBranchNotFound, branchName)
	}
	newRef := plumbing.NewHashReference(refName, plumbing.NewHash(targetHash))
	if err := m.repo.repo.Storer.SetReference(newRef); err != nil {
		return fmt.Errorf("fast-forward: %w", err)
	}
	return nil
}

// CreateMergeCommit creates a commit on the current HEAD with two parents
// (HEAD's previous commit + the source commit). The commit reuses HEAD's
// tree, so the merge is metadata-only — useful for --no-ff merges where
// the worktree state already reflects the desired result.
// Refs: FR-8.4
func (m *MergeStore) CreateMergeCommit(_ context.Context, message, sourceHash string) (string, error) {
	goRepo := m.repo.repo

	headRef, err := goRepo.Head()
	if err != nil {
		return "", fmt.Errorf("merge commit: resolve HEAD: %w", err)
	}
	headHash := headRef.Hash()

	wt, err := goRepo.Worktree()
	if err != nil {
		return "", fmt.Errorf("merge commit: get worktree: %w", err)
	}

	commitHash, err := wt.Commit(message, &gogit.CommitOptions{
		Author: &object.Signature{
			Name:  "mgit-merge",
			Email: "mgit-merge@mgit",
			When:  m.repo.Now(),
		},
		Parents:           []plumbing.Hash{headHash, plumbing.NewHash(sourceHash)},
		AllowEmptyCommits: true,
	})
	if err != nil {
		return "", fmt.Errorf("merge commit: create: %w", err)
	}

	// Update the current branch ref to point at the new merge commit.
	newRef := plumbing.NewHashReference(headRef.Name(), commitHash)
	if err := goRepo.Storer.SetReference(newRef); err != nil {
		return "", fmt.Errorf("merge commit: update ref: %w", err)
	}
	return commitHash.String(), nil
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
