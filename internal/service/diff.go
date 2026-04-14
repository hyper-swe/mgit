package service

import (
	"context"
	"fmt"
	"strings"

	"github.com/hyper-swe/mgit-dev/internal/model"
	gitstore "github.com/hyper-swe/mgit-dev/internal/store/git"
	"github.com/hyper-swe/mgit-dev/internal/store/index"
)

// DiffService computes diffs between commits and across tasks.
// Refs: FR-11, MGIT-3.2.1
type DiffService struct {
	diffStore   *gitstore.DiffStore
	commitStore *gitstore.CommitStore
	indexStore  *index.Store
}

// NewDiffService creates a DiffService with injected dependencies.
func NewDiffService(ds *gitstore.DiffStore, cs *gitstore.CommitStore, idx *index.Store) *DiffService {
	return &DiffService{
		diffStore:   ds,
		commitStore: cs,
		indexStore:  idx,
	}
}

// DiffCommits computes the file-level diff between two commits.
// Refs: FR-11
func (s *DiffService) DiffCommits(ctx context.Context, fromHash, toHash string) ([]model.FileDiff, error) {
	diffs, err := s.diffStore.DiffCommits(ctx, fromHash, toHash)
	if err != nil {
		return nil, fmt.Errorf("diff commits: %w", err)
	}
	return diffs, nil
}

// DiffTask computes the cumulative diff for all commits in a task.
// Returns the diff from before the task's first commit to after its last.
// Refs: FR-11
func (s *DiffService) DiffTask(ctx context.Context, taskID string) ([]model.FileDiff, error) {
	records, err := s.indexStore.GetTaskCommits(ctx, taskID)
	if err != nil {
		return nil, fmt.Errorf("diff task %s: get commits: %w", taskID, err)
	}
	if len(records) == 0 {
		return nil, fmt.Errorf("diff task %s: %w", taskID, model.ErrTaskNotFound)
	}

	// Get the first commit's parent as the "before" state
	firstCommit, err := s.commitStore.GetCommit(ctx, records[0].CommitHash)
	if err != nil {
		return nil, fmt.Errorf("diff task %s: get first commit: %w", taskID, err)
	}

	lastHash := records[len(records)-1].CommitHash

	if firstCommit.ParentID == "" {
		// First commit has no parent — return empty diff
		return []model.FileDiff{}, nil
	}

	return s.diffStore.DiffCommits(ctx, firstCommit.ParentID, lastHash)
}

// DiffRange computes the diff over a range of commits.
// Refs: FR-11
func (s *DiffService) DiffRange(ctx context.Context, fromHash, toHash string) ([]model.FileDiff, error) {
	return s.diffStore.DiffCommits(ctx, fromHash, toHash)
}

// Statistics computes aggregate line change counts for a set of diffs.
// Refs: FR-11
func (s *DiffService) Statistics(diffs []model.FileDiff) model.DiffStatistics {
	var stats model.DiffStatistics
	for _, d := range diffs {
		s := d.Statistics()
		stats.LinesAdded += s.LinesAdded
		stats.LinesRemoved += s.LinesRemoved
	}
	return stats
}

// FormatUnified renders a slice of FileDiffs as a unified diff text suitable
// for terminal display. Each file is shown with a "diff --mgit a/path b/path"
// header followed by an operation marker and any hunk content.
// Refs: FR-11
func (s *DiffService) FormatUnified(diffs []model.FileDiff) string {
	var b strings.Builder
	for _, d := range diffs {
		fmt.Fprintf(&b, "diff --mgit a/%s b/%s\n", d.Path, d.Path)
		switch d.Operation {
		case model.DiffAdded:
			fmt.Fprintf(&b, "new file: %s\n", d.Path)
		case model.DiffDeleted:
			fmt.Fprintf(&b, "deleted file: %s\n", d.Path)
		case model.DiffModified:
			fmt.Fprintf(&b, "modified: %s\n", d.Path)
		case model.DiffRenamed:
			fmt.Fprintf(&b, "renamed: %s\n", d.Path)
		}
		if d.OldHash != "" || d.NewHash != "" {
			fmt.Fprintf(&b, "--- a/%s (%s)\n", d.Path, shortHash(d.OldHash))
			fmt.Fprintf(&b, "+++ b/%s (%s)\n", d.Path, shortHash(d.NewHash))
		}
		for _, h := range d.Hunks {
			fmt.Fprintf(&b, "@@ -%d,%d +%d,%d @@\n",
				h.LineStart, h.LinesRemoved, h.LineStart, h.LinesAdded)
			if h.Content != "" {
				b.WriteString(h.Content)
				if !strings.HasSuffix(h.Content, "\n") {
					b.WriteString("\n")
				}
			}
		}
	}
	return b.String()
}

// FormatStat renders a per-file summary line for each diff plus an aggregate
// totals line, similar to "git diff --stat".
// Refs: FR-11
func (s *DiffService) FormatStat(diffs []model.FileDiff) string {
	var b strings.Builder
	stats := s.Statistics(diffs)
	for _, d := range diffs {
		fileStats := d.Statistics()
		fmt.Fprintf(&b, " %-40s | %4d +%d -%d\n",
			d.Path,
			fileStats.LinesAdded+fileStats.LinesRemoved,
			fileStats.LinesAdded,
			fileStats.LinesRemoved)
	}
	fmt.Fprintf(&b, " %d files changed, %d insertions(+), %d deletions(-)\n",
		len(diffs), stats.LinesAdded, stats.LinesRemoved)
	return b.String()
}

// shortHash returns the first 8 characters of a hash, or an empty string.
func shortHash(h string) string {
	if len(h) <= 8 {
		return h
	}
	return h[:8]
}
