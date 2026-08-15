package service

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/hyper-swe/mgit/internal/model"
	gitstore "github.com/hyper-swe/mgit/internal/store/git"
	"github.com/hyper-swe/mgit/internal/store/index"
)

// cadenceEnv is a store whose clock the test drives, so a trail can be laid
// down at the exact instants a real run produced.
type cadenceEnv struct {
	idx    *index.Store
	commit *CommitService
	now    time.Time
}

func setupCadenceEnv(t *testing.T) *cadenceEnv {
	t.Helper()
	tmpDir := t.TempDir()
	env := &cadenceEnv{now: time.Date(2026, 8, 12, 11, 48, 35, 0, time.UTC)}
	clock := func() time.Time { return env.now }

	repo, err := gitstore.Init(tmpDir, clock)
	require.NoError(t, err)
	t.Cleanup(func() { _ = repo.Close() })

	idx, err := index.New(filepath.Join(tmpDir, ".mgit", "index.db"), clock)
	require.NoError(t, err)
	t.Cleanup(func() { _ = idx.Close() })

	env.idx = idx
	env.commit = NewCommitService(repo, gitstore.NewCommitStore(repo), idx)
	return env
}

// registerWorktree writes the worktrees row that `mgit work` writes, at the
// current clock instant. That row's created_at IS the denominator.
func (e *cadenceEnv) registerWorktree(t *testing.T, taskID, at string) {
	t.Helper()
	e.now = mustTime(t, at)
	require.NoError(t, e.idx.InsertWorktree(context.Background(), &model.WorktreeInfo{
		Path: "/tmp/wt-" + taskID, Branch: "task/" + taskID, TaskID: taskID, AgentID: "agent",
	}))
}

// appendCommit writes one task_commits row at a chosen instant with a chosen
// author, mirroring what commit and squash each record.
func (e *cadenceEnv) appendCommit(t *testing.T, taskID, agentID, at string, pos int) {
	t.Helper()
	e.now = mustTime(t, at)
	require.NoError(t, e.idx.AppendTaskCommit(context.Background(), index.TaskCommitInsert{
		TaskID: taskID, CommitHash: "hash" + at, ContentHash: "content" + at,
		AgentID: agentID, Position: pos,
	}))
}

func mustTime(t *testing.T, s string) time.Time {
	t.Helper()
	parsed, err := time.Parse(time.RFC3339, s)
	require.NoError(t, err)
	return parsed
}

// TestCommitService_TaskCadence_MGIT102Shape_ReportsPackagedPostHoc replays
// MGIT-102's real trail through the real store: six authored commits inside
// the last five minutes of a forty-minute run, plus the squash artifact that
// followed them. Refs: MGIT-110, R-H234
func TestCommitService_TaskCadence_MGIT102Shape_ReportsPackagedPostHoc(t *testing.T) {
	env := setupCadenceEnv(t)
	const task = "MGIT-102"
	env.registerWorktree(t, task, "2026-08-12T11:48:35Z")
	for i, at := range []string{
		"2026-08-12T12:23:40Z", "2026-08-12T12:23:53Z", "2026-08-12T12:24:04Z",
		"2026-08-12T12:24:15Z", "2026-08-12T12:24:25Z", "2026-08-12T12:28:21Z",
	} {
		env.appendCommit(t, task, "cli", at, i)
	}
	env.appendCommit(t, task, "mgit-squash", "2026-08-12T12:30:09Z", 6)

	ev, err := env.commit.TaskCadence(context.Background(), task)

	require.NoError(t, err)
	assert.Equal(t, model.CadencePackagedPostHoc, ev.Verdict)
	assert.Equal(t, 6, ev.Commits, "the squash artifact is not one of the agent's steps")
	require.NotNil(t, ev.Measure)
	assert.Equal(t, "2026-08-12T12:28:21Z", ev.Measure.LastCommitAt,
		"the window must end at the last AUTHORED commit, not at hand-off")
}

// TestCommitService_TaskCadence_MGIT112Shape_ReportsSpread is the contrast
// case the label has to tell apart from the one above. Refs: MGIT-110
func TestCommitService_TaskCadence_MGIT112Shape_ReportsSpread(t *testing.T) {
	env := setupCadenceEnv(t)
	const task = "MGIT-112"
	env.registerWorktree(t, task, "2026-08-15T10:52:50Z")
	for i, at := range []string{
		"2026-08-15T11:01:23Z", "2026-08-15T11:06:48Z", "2026-08-15T11:13:58Z",
		"2026-08-15T11:22:56Z", "2026-08-15T11:24:07Z",
	} {
		env.appendCommit(t, task, "cli", at, i)
	}
	env.appendCommit(t, task, "mgit-squash", "2026-08-15T11:27:51Z", 5)

	ev, err := env.commit.TaskCadence(context.Background(), task)

	require.NoError(t, err)
	assert.Equal(t, model.CadenceSpreadAcrossRun, ev.Verdict)
	assert.Equal(t, 5, ev.Commits)
}

// TestCommitService_TaskCadence_MGIT105Shape_ReportsSingleCheckpoint replays
// the commonest shape in this repo's history, and it is where the two halves
// of this feature interact: the index holds two rows, but one is the squash.
// Counting it gives a 64-second window over a 16-minute run and reports
// PACKAGED_POST_HOC — a manufactured-trail accusation built entirely out of
// mgit's own hand-off commit. Excluding it leaves one authored commit, which
// is its own observation. Refs: MGIT-110, MGIT-22, R-H234
func TestCommitService_TaskCadence_MGIT105Shape_ReportsSingleCheckpoint(t *testing.T) {
	env := setupCadenceEnv(t)
	const task = "MGIT-105"
	env.registerWorktree(t, task, "2026-08-12T17:39:31Z")
	env.appendCommit(t, task, "cli", "2026-08-12T17:55:56Z", 0)
	env.appendCommit(t, task, "mgit-squash", "2026-08-12T17:57:00Z", 1)

	ev, err := env.commit.TaskCadence(context.Background(), task)

	require.NoError(t, err)
	assert.Equal(t, model.CadenceSingleCheckpoint, ev.Verdict)
	assert.Empty(t, ev.Reason, "this is an observation, not a refusal")
	assert.Equal(t, 1, ev.Commits)
	require.NotNil(t, ev.Measure)
	assert.InDelta(t, 985.0, ev.Measure.ElapsedSeconds, 0.5, "~16.4 minutes of run")
}

// TestCommitService_TaskCadence_MGIT109Shape_ReportsNoCommits: the interrupted
// run. Zero commits is reported as the complete fact it is. Refs: MGIT-110, MGIT-109
func TestCommitService_TaskCadence_MGIT109Shape_ReportsNoCommits(t *testing.T) {
	env := setupCadenceEnv(t)
	env.registerWorktree(t, "MGIT-109", "2026-08-12T18:22:38Z")

	ev, err := env.commit.TaskCadence(context.Background(), "MGIT-109")

	require.NoError(t, err)
	assert.Equal(t, model.CadenceNoCommits, ev.Verdict)
	assert.Equal(t, 0, ev.Commits)
	assert.Contains(t, ev.Summary, "2026-08-12T18:22:38Z",
		"naming the worktree's creation makes the silence measurable")
}

// TestCommitService_TaskCadence_SquashOnlyTrail_IsNotEvidenceOfWork: a task
// whose only index rows are derived artifacts has NO authored trail. Counting
// the squash would report a healthy-looking single commit for a run that
// checkpointed nothing. Refs: MGIT-110, MGIT-22
func TestCommitService_TaskCadence_SquashOnlyTrail_IsNotEvidenceOfWork(t *testing.T) {
	env := setupCadenceEnv(t)
	const task = "MGIT-901"
	env.registerWorktree(t, task, "2026-08-15T09:00:00Z")
	env.appendCommit(t, task, "mgit-squash", "2026-08-15T09:40:00Z", 0)

	ev, err := env.commit.TaskCadence(context.Background(), task)

	require.NoError(t, err)
	assert.Equal(t, model.CadenceNoCommits, ev.Verdict)
	assert.Equal(t, 0, ev.Commits)
}

// TestCommitService_TaskCadence_NoWorktreeRegistered_RefusesToLabel: a task
// committed without `mgit work` has no recorded run start, and the service
// must refuse rather than substitute the first commit for it — that
// denominator would make every trail look like complete coverage.
// Refs: MGIT-110, FR-16
func TestCommitService_TaskCadence_NoWorktreeRegistered_RefusesToLabel(t *testing.T) {
	env := setupCadenceEnv(t)
	const task = "MGIT-902"
	env.appendCommit(t, task, "cli", "2026-08-15T09:40:00Z", 0)
	env.appendCommit(t, task, "cli", "2026-08-15T09:40:13Z", 1)

	ev, err := env.commit.TaskCadence(context.Background(), task)

	require.NoError(t, err)
	assert.Equal(t, model.CadenceInsufficientEvidence, ev.Verdict)
	assert.Equal(t, model.CadenceReasonNoDenominator, ev.Reason)
	assert.Nil(t, ev.Measure)
}

// TestParseCommitTimes_UnparseableTimestamp_ErrorsRatherThanDropsTheRow: a
// created_at the index cannot parse is an integrity problem, not an evidence
// gap. Silently skipping the row would shorten the trail and could flip the
// verdict without anyone being told. Refs: MGIT-110, FR-12
func TestParseCommitTimes_UnparseableTimestamp_ErrorsRatherThanDropsTheRow(t *testing.T) {
	_, err := parseCommitTimes([]index.CommitRecord{
		{AgentID: "cli", CommitHash: "abc1234", CreatedAt: "2026-08-15T09:00:00Z"},
		{AgentID: "cli", CommitHash: "def5678", CreatedAt: "12 August, quarter past"},
	})

	require.Error(t, err)
	assert.Contains(t, err.Error(), "def5678", "the unreadable row is named")
}

// TestParseCommitTimes_DerivedAuthors_AreNotPartOfTheTrail keeps the
// exclusion visible at the layer that applies it. Refs: MGIT-110, MGIT-22
func TestParseCommitTimes_DerivedAuthors_AreNotPartOfTheTrail(t *testing.T) {
	got, err := parseCommitTimes([]index.CommitRecord{
		{AgentID: "cli", CreatedAt: "2026-08-15T09:00:00Z"},
		{AgentID: "mgit-rollback", CreatedAt: "2026-08-15T09:10:00Z"},
		{AgentID: "mgit-squash", CreatedAt: "2026-08-15T09:20:00Z"},
		{AgentID: "mgit-merge", CreatedAt: "2026-08-15T09:30:00Z"},
	})

	require.NoError(t, err)
	assert.Len(t, got, 2, "the rollback is a decision taken during the run; squash and merge are not")
}

// TestCommitService_TaskCadence_IndexUnavailable_FailsRatherThanLabels: a
// label computed from a store that could not be read would be a confident
// wrong answer, which is the defect class this whole ticket is about.
// Refs: MGIT-110
func TestCommitService_TaskCadence_IndexUnavailable_FailsRatherThanLabels(t *testing.T) {
	env := setupCadenceEnv(t)
	env.registerWorktree(t, "MGIT-904", "2026-08-15T09:00:00Z")
	require.NoError(t, env.idx.Close())

	ev, err := env.commit.TaskCadence(context.Background(), "MGIT-904")

	require.Error(t, err)
	assert.Nil(t, ev)
	assert.Contains(t, err.Error(), "MGIT-904", "the failure names the task it was asked about")
}

// TestCommitService_TaskCadence_UnknownTask_IsNoCommitsNotAnError: asking
// about a task that was never worked is answered, not refused — "nothing was
// recorded" is the answer. Refs: MGIT-110
func TestCommitService_TaskCadence_UnknownTask_IsNoCommitsNotAnError(t *testing.T) {
	env := setupCadenceEnv(t)

	ev, err := env.commit.TaskCadence(context.Background(), "MGIT-000")

	require.NoError(t, err)
	assert.Equal(t, model.CadenceNoCommits, ev.Verdict)
}
