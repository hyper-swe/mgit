package service

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/hyper-swe/mgit/internal/model"
	"github.com/hyper-swe/mgit/internal/store/index"
)

// TaskCadence assembles the commit-cadence evidence label for one task: was
// its commit trail written as the work happened, or packaged at hand-off?
//
// It reads only what the index already holds — the task's commit timestamps
// and the created_at of the worktree bound to the task — and combines them in
// model.ClassifyCadence. Nothing here judges quality, nothing is scored, and
// nothing in mgit gates on the result: it is a label a reviewer reads, and a
// quota an agent could satisfy would manufacture the very trail it exists to
// expose (R-H234).
//
// A task with no registered worktree yields INSUFFICIENT_EVIDENCE rather than
// a substituted denominator. Using the first commit as the run's start would
// make every trail look like complete coverage of itself.
// Refs: MGIT-110, R-H234, FR-4, FR-16
func (s *CommitService) TaskCadence(ctx context.Context, taskID string) (*model.CadenceEvidence, error) {
	records, err := s.indexStore.GetTaskCommits(ctx, taskID)
	if err != nil {
		return nil, fmt.Errorf("task cadence %s: %w", taskID, err)
	}
	commitTimes, err := parseCommitTimes(records)
	if err != nil {
		return nil, fmt.Errorf("task cadence %s: %w", taskID, err)
	}

	obs := model.CadenceObservation{CommitTimes: commitTimes}
	wt, err := s.indexStore.GetWorktreeByTask(ctx, taskID)
	switch {
	case err == nil:
		obs.RunStart, obs.HasRunStart = wt.CreatedAt, true
	case errors.Is(err, model.ErrWorktreeNotFound):
		// Not an error: the task was committed without `mgit work`, so the
		// denominator is genuinely absent and the classifier will say so.
	default:
		// Defensive today: GetWorktreeByTask currently reports EVERY failure,
		// including a store-level one, as ErrWorktreeNotFound, so a broken
		// index degrades to "no denominator" rather than reaching here. That
		// degradation is the humble direction (it refuses to label), but the
		// masking is a store wart — see MGIT-116. This branch is what should
		// happen once the store distinguishes the two.
		return nil, fmt.Errorf("task cadence %s: worktree: %w", taskID, err)
	}

	evidence := model.ClassifyCadence(obs)
	return &evidence, nil
}

// parseCommitTimes extracts the creation instants of a task's AUTHORED
// commits, dropping the derived artifacts (squash, merge) that restate
// existing work at integration time.
//
// An unparseable created_at is returned as an error rather than skipped: it
// is an index-integrity problem, and silently shortening a trail would change
// the verdict without telling anyone. Refs: MGIT-110, FR-12
func parseCommitTimes(records []index.CommitRecord) ([]time.Time, error) {
	out := make([]time.Time, 0, len(records))
	for _, r := range records {
		if model.IsDerivedCommitAuthor(r.AgentID) {
			continue
		}
		t, err := time.Parse(time.RFC3339, r.CreatedAt)
		if err != nil {
			return nil, fmt.Errorf("parse created_at %q for commit %s: %w",
				r.CreatedAt, r.CommitHash, err)
		}
		out = append(out, t)
	}
	return out, nil
}
