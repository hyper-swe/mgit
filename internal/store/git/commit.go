package git

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"

	gogit "github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/object"

	"github.com/astutic/mgit/internal/model"
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
	c.ContentHash = computeContentHash(c)

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

// computeContentHash computes the SHA-256 content hash for a commit.
// This is mgit's own integrity hash per ADR-002 (dual-hash model).
// Input: message + file_diffs JSON + task_id + parent_content_hash + created_at
// Refs: ADR-002, NFR-5
func computeContentHash(c *model.Commit) string {
	h := sha256.New()
	h.Write([]byte(c.Message))

	diffsJSON, _ := json.Marshal(c.FileDiffs) //nolint:errcheck // marshal of known type
	h.Write(diffsJSON)

	h.Write([]byte(c.TaskID.String()))
	h.Write([]byte(c.ParentID))
	h.Write([]byte(c.CreatedAt.UTC().Format("2006-01-02T15:04:05Z")))

	return fmt.Sprintf("%x", h.Sum(nil))
}
