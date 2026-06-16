package git

import (
	"context"
	"fmt"

	gogit "github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/object"

	"github.com/hyper-swe/mgit/internal/model"
)

// CommitStore manages commit objects in the go-git store.
// It creates go-git commit objects and computes SHA-256 content hashes
// per the dual-hash model (ADR-002).
// Refs: FR-2, FR-3, ADR-002, MGIT-2.2.2
type CommitStore struct {
	repo *Repository
}

// NewCommitStore creates a CommitStore backed by the given Repository.
func NewCommitStore(repo *Repository) *CommitStore {
	return &CommitStore{repo: repo}
}

// CreateCommit creates a new commit in the go-git object store.
// It populates the commit's CommitID (SHA-1) and ContentHash (SHA-256),
// sets the timestamp from the injected clock, and stores the commit.
// The commit is created on the current HEAD.
// Refs: FR-2, FR-3, ADR-002
func (cs *CommitStore) CreateCommit(_ context.Context, c *model.Commit) (string, error) {
	goRepo := cs.repo.repo

	// Set timestamp from injected clock
	c.CreatedAt = cs.repo.Now()

	// Get current HEAD to set as parent
	headRef, err := goRepo.Head()
	if err != nil {
		return "", fmt.Errorf("resolve HEAD: %w", err)
	}
	parentHash := headRef.Hash()
	c.ParentID = parentHash.String()

	// Create go-git commit using worktree
	wt, err := goRepo.Worktree()
	if err != nil {
		return "", fmt.Errorf("get worktree: %w", err)
	}

	commitHash, err := wt.Commit(c.Message, &gogit.CommitOptions{
		Author: &object.Signature{
			Name:  c.AgentID,
			Email: c.AgentID + "@mgit",
			When:  c.CreatedAt,
		},
		Parents:           []plumbing.Hash{parentHash},
		AllowEmptyCommits: true,
	})
	if err != nil {
		return "", fmt.Errorf("create go-git commit: %w", err)
	}

	// Set SHA-1 commit ID (go-git native)
	sha1Hex := commitHash.String()
	c.CommitID = sha1Hex

	// Compute SHA-256 content hash (mgit integrity per ADR-002)
	c.ContentHash = c.ComputeContentHash()

	// Update HEAD ref
	ref := plumbing.NewHashReference(
		headRef.Name(),
		commitHash,
	)
	if err := goRepo.Storer.SetReference(ref); err != nil {
		return "", fmt.Errorf("update ref: %w", err)
	}

	return sha1Hex, nil
}

// GetCommit retrieves a commit by its SHA-1 hash from the go-git store.
// Returns ErrCommitNotFound if the hash does not exist.
// Refs: FR-3
func (cs *CommitStore) GetCommit(_ context.Context, hash string) (*model.Commit, error) {
	goRepo := cs.repo.repo

	h := plumbing.NewHash(hash)
	obj, err := goRepo.CommitObject(h)
	if err != nil {
		return nil, fmt.Errorf("%w: %s", model.ErrCommitNotFound, hash)
	}

	return commitFromGitObject(obj), nil
}

// ListCommits returns all commits reachable from HEAD.
// Refs: FR-3
func (cs *CommitStore) ListCommits(_ context.Context) ([]*model.Commit, error) {
	goRepo := cs.repo.repo

	iter, err := goRepo.Log(&gogit.LogOptions{
		Order: gogit.LogOrderCommitterTime,
	})
	if err != nil {
		return nil, fmt.Errorf("list commits: %w", err)
	}

	var commits []*model.Commit
	err = iter.ForEach(func(c *object.Commit) error {
		commits = append(commits, commitFromGitObject(c))
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("iterate commits: %w", err)
	}

	return commits, nil
}

// DeleteCommit always returns ErrAppendOnlyViolation.
// Commits are immutable per FR-12 append-only requirement.
// Refs: FR-12
func (cs *CommitStore) DeleteCommit(_ context.Context, _ string) error {
	return fmt.Errorf("%w: commits are immutable", model.ErrAppendOnlyViolation)
}

// GetFileFromCommit reads the contents of a single file from the tree
// of the given commit. Returns model.ErrCommitNotFound if the commit
// hash does not exist, or model.ErrFileNotFound if the path is absent
// from the commit tree.
// Refs: FR-6.7, MGIT-4.2.8
func (cs *CommitStore) GetFileFromCommit(_ context.Context, hash, path string) ([]byte, error) {
	if path == "" {
		return nil, fmt.Errorf("get file: path must not be empty")
	}

	goRepo := cs.repo.repo

	commitObj, err := goRepo.CommitObject(plumbing.NewHash(hash))
	if err != nil {
		return nil, fmt.Errorf("%w: %s", model.ErrCommitNotFound, hash)
	}

	tree, err := commitObj.Tree()
	if err != nil {
		return nil, fmt.Errorf("get tree for commit %s: %w", hash, err)
	}

	file, err := tree.File(path)
	if err != nil {
		return nil, fmt.Errorf("%w: %s in commit %s", model.ErrFileNotFound, path, hash)
	}

	contents, err := file.Contents()
	if err != nil {
		return nil, fmt.Errorf("read file %s in commit %s: %w", path, hash, err)
	}

	return []byte(contents), nil
}

// commitFromGitObject converts a go-git commit object to a model.Commit.
// This wraps the go-git type so it is never exposed to callers.
func commitFromGitObject(obj *object.Commit) *model.Commit {
	parentID := ""
	if obj.NumParents() > 0 {
		parentID = obj.ParentHashes[0].String()
	}

	return &model.Commit{
		CommitID:  obj.Hash.String(),
		Message:   obj.Message,
		CreatedAt: obj.Author.When,
		ParentID:  parentID,
		TreeHash:  obj.TreeHash.String(),
		AgentID:   obj.Author.Name,
		CreatedBy: obj.Author.Name,
	}
}

// content_hash is computed by model.Commit.ComputeContentHash (ADR-002),
// the single authoritative definition shared with sandbox land
// re-verification (FR-17.24).
