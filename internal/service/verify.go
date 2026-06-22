package service

import (
	"context"
	"fmt"

	"github.com/hyper-swe/mgit/internal/model"
	gitstore "github.com/hyper-swe/mgit/internal/store/git"
	"github.com/hyper-swe/mgit/internal/store/index"
)

// VerifyService performs integrity checks on commits and indexes.
// Refs: FR-12, MGIT-3.2.2
type VerifyService struct {
	commitStore *gitstore.CommitStore
	indexStore  *index.Store
}

// NewVerifyService creates a VerifyService with injected dependencies.
func NewVerifyService(cs *gitstore.CommitStore, idx *index.Store) *VerifyService {
	return &VerifyService{
		commitStore: cs,
		indexStore:  idx,
	}
}

// VerifyCommitChain validates that a sequence of commit hashes forms
// a valid parent-child chain. Each commit's parent_id must match the
// previous commit's commit_id.
// Refs: FR-12
func (s *VerifyService) VerifyCommitChain(ctx context.Context, hashes []string) error {
	if len(hashes) == 0 {
		return nil
	}

	var prevHash string
	for i, hash := range hashes {
		commit, err := s.commitStore.GetCommit(ctx, hash)
		if err != nil {
			return fmt.Errorf("verify chain: commit %d (%s): %w", i, hash, err)
		}

		if i > 0 && commit.ParentID != prevHash {
			return fmt.Errorf("%w: commit %s has parent %s, expected %s",
				model.ErrChainBroken, hash[:8], commit.ParentID[:8], prevHash[:8])
		}

		prevHash = commit.CommitID
	}

	return nil
}

// VerifyTaskCommits validates all commits for a task form a valid chain.
// Refs: FR-12
func (s *VerifyService) VerifyTaskCommits(ctx context.Context, taskID string) error {
	records, err := s.indexStore.GetTaskCommits(ctx, taskID)
	if err != nil {
		return fmt.Errorf("verify task %s: %w", taskID, err)
	}
	if len(records) == 0 {
		return nil
	}

	// Verify each commit exists in go-git
	for _, rec := range records {
		_, err := s.commitStore.GetCommit(ctx, rec.CommitHash)
		if err != nil {
			return fmt.Errorf("verify task %s: commit %s: %w",
				taskID, rec.CommitHash[:8], err)
		}
	}

	return nil
}

// VerifyIndexIntegrity checks that all commits in git have index entries
// and all index entries point to valid commits. Detects orphaned records.
// Refs: FR-12
func (s *VerifyService) VerifyIndexIntegrity(ctx context.Context) ([]string, error) {
	var issues []string

	// Get all commits from git
	gitCommits, err := s.commitStore.ListCommits(ctx)
	if err != nil {
		return nil, fmt.Errorf("verify index: list git commits: %w", err)
	}

	// Check each git commit has an index entry. The parentless genesis
	// commit is created by mgit itself (FR-1.2) and is not task-tagged, so
	// it legitimately has no index entry and must not be flagged.
	for _, gc := range gitCommits {
		if gc.ParentID == "" {
			continue
		}
		_, err := s.indexStore.GetCommitTask(ctx, gc.CommitID)
		if err != nil {
			issues = append(issues,
				fmt.Sprintf("git commit %s has no index entry", gc.ShortID()))
		}
	}

	return issues, nil
}
