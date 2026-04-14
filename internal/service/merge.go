package service

import (
	"context"
	"fmt"
	"strings"

	"github.com/hyper-swe/mgit-dev/internal/model"
	gitstore "github.com/hyper-swe/mgit-dev/internal/store/git"
)

// MergeStrategy controls how the source branch is integrated into HEAD.
// Refs: FR-8.4, MGIT-4.2.10
type MergeStrategy string

const (
	// MergeAuto uses fast-forward when HEAD is an ancestor of source,
	// otherwise creates a two-parent merge commit.
	MergeAuto MergeStrategy = "auto"
	// MergeNoFF always creates a two-parent merge commit even when a
	// fast-forward is possible.
	MergeNoFF MergeStrategy = "no-ff"
	// MergeSquash collapses source-only commits into a single commit on
	// HEAD with one parent (the previous HEAD).
	MergeSquash MergeStrategy = "squash"
)

// MergeRequest holds the parameters for a merge operation.
// Refs: FR-8.4, MGIT-4.2.10
type MergeRequest struct {
	SourceBranch string
	Strategy     MergeStrategy
	Message      string
}

// MergeResult holds the outcome of a merge operation, suitable for JSON
// serialization to CLI consumers.
// Refs: MGIT-4.2.10
type MergeResult struct {
	Strategy   MergeStrategy `json:"strategy"`
	Source     string        `json:"source"`
	Target     string        `json:"target"`
	MergedHash string        `json:"merged_hash"`
	FastFwd    bool          `json:"fast_forward"`
	Status     string        `json:"status"`
}

// MergeService integrates a source branch into the current branch using a
// fast-forward, no-ff, or squash strategy. Conflicts are detected before
// any commit is created and surfaced as model.ErrMergeConflict.
// Refs: FR-8.4, MGIT-4.2.10
type MergeService struct {
	branches *gitstore.BranchStore
	merge    *gitstore.MergeStore
	commits  *gitstore.CommitStore
	repo     *gitstore.Repository
}

// NewMergeService creates a MergeService with the given store dependencies.
func NewMergeService(repo *gitstore.Repository, bs *gitstore.BranchStore, ms *gitstore.MergeStore, cs *gitstore.CommitStore) *MergeService {
	return &MergeService{
		branches: bs,
		merge:    ms,
		commits:  cs,
		repo:     repo,
	}
}

// Merge integrates SourceBranch into the current HEAD branch using the
// chosen strategy. Returns model.ErrMergeConflict if both branches modify
// the same file with different content.
// Refs: FR-8.4
func (s *MergeService) Merge(ctx context.Context, req MergeRequest) (*MergeResult, error) {
	if req.SourceBranch == "" {
		return nil, fmt.Errorf("merge: source branch must not be empty")
	}
	strategy := req.Strategy
	if strategy == "" {
		strategy = MergeAuto
	}

	// Resolve source branch tip + current HEAD.
	src, err := s.branches.GetBranch(ctx, req.SourceBranch)
	if err != nil {
		return nil, fmt.Errorf("merge: %w", err)
	}
	headHash, err := s.repo.Head()
	if err != nil {
		return nil, fmt.Errorf("merge: resolve HEAD: %w", err)
	}
	currentBranch, err := s.repo.CurrentBranch()
	if err != nil {
		return nil, fmt.Errorf("merge: current branch: %w", err)
	}

	// Conflict detection (skipped only for fast-forward, where there is
	// nothing to merge — HEAD is already an ancestor of the source).
	canFF, err := s.merge.IsAncestor(ctx, headHash, src.HeadCommit)
	if err != nil {
		return nil, fmt.Errorf("merge: ancestor check: %w", err)
	}
	if !canFF {
		base, err := s.merge.MergeBase(ctx, headHash, src.HeadCommit)
		if err != nil {
			return nil, fmt.Errorf("merge: merge-base: %w", err)
		}
		if base == "" {
			return nil, fmt.Errorf("merge: no common ancestor between HEAD and %s", req.SourceBranch)
		}
		conflicts, err := s.merge.ConflictingPaths(ctx, base, headHash, src.HeadCommit)
		if err != nil {
			return nil, fmt.Errorf("merge: conflict scan: %w", err)
		}
		if len(conflicts) > 0 {
			return nil, fmt.Errorf("merge: %w: %s",
				model.ErrMergeConflict, strings.Join(conflicts, ", "))
		}
	}

	message := req.Message
	if message == "" {
		message = fmt.Sprintf("Merge branch %q into %s", req.SourceBranch, currentBranch)
	}

	switch strategy {
	case MergeAuto:
		if canFF {
			if err := s.merge.FastForward(ctx, currentBranch, src.HeadCommit); err != nil {
				return nil, fmt.Errorf("merge: %w", err)
			}
			return &MergeResult{
				Strategy:   MergeAuto,
				Source:     req.SourceBranch,
				Target:     currentBranch,
				MergedHash: src.HeadCommit,
				FastFwd:    true,
				Status:     "fast-forward",
			}, nil
		}
		hash, err := s.merge.CreateMergeCommit(ctx, message, src.HeadCommit)
		if err != nil {
			return nil, fmt.Errorf("merge: %w", err)
		}
		return &MergeResult{
			Strategy: MergeAuto, Source: req.SourceBranch, Target: currentBranch,
			MergedHash: hash, FastFwd: false, Status: "merged",
		}, nil

	case MergeNoFF:
		hash, err := s.merge.CreateMergeCommit(ctx, message, src.HeadCommit)
		if err != nil {
			return nil, fmt.Errorf("merge: %w", err)
		}
		return &MergeResult{
			Strategy: MergeNoFF, Source: req.SourceBranch, Target: currentBranch,
			MergedHash: hash, FastFwd: false, Status: "merged",
		}, nil

	case MergeSquash:
		// Squash merge: collect changes from source's task ID and create a
		// single commit on HEAD that references only the previous HEAD.
		taskID := src.TaskID.String()
		squashMsg := req.Message
		if squashMsg == "" {
			squashMsg = fmt.Sprintf("Squash merge branch %q", req.SourceBranch)
		}
		c, err := s.commits.CreateCommit(ctx, &model.Commit{
			TaskID:     src.TaskID,
			AgentID:    "mgit-merge",
			Message:    squashMsg,
			CommitType: model.CommitTypeMerge,
			CreatedBy:  "mgit-merge",
			Branch:     currentBranch,
		})
		if err != nil {
			return nil, fmt.Errorf("merge --squash: %w", err)
		}
		_ = taskID // taskID is implied by src.TaskID
		return &MergeResult{
			Strategy: MergeSquash, Source: req.SourceBranch, Target: currentBranch,
			MergedHash: c, FastFwd: false, Status: "squashed",
		}, nil

	default:
		return nil, fmt.Errorf("merge: unknown strategy %q", strategy)
	}
}
