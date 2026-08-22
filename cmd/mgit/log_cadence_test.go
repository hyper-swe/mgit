package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/hyper-swe/mgit/internal/model"
	"github.com/hyper-swe/mgit/internal/store/index"
)

// seedCadenceRepo initializes a repo in a temp cwd and records two commits
// for taskID, the way an agent that only commits at hand-off would.
func seedCadenceRepo(t *testing.T, taskID string) string {
	t.Helper()
	repo := t.TempDir()
	t.Chdir(repo)
	require.NoError(t, runCLI(t, "init"))

	for _, step := range []string{"one", "two"} {
		require.NoError(t, os.WriteFile(filepath.Join(repo, step+".txt"), []byte(step+"\n"), 0o600))
		require.NoError(t, runCLI(t, "commit", "-a", "--task-id", taskID, "-m", "step "+step))
	}
	return repo
}

// backdateWorktree registers a worktree for taskID as though `mgit work` had
// provisioned it `ago` before now — the denominator the label rests on.
//
// It opens the index directly because the CLI cannot create a past: the
// commits above are stamped at real now, so a run of a realistic LENGTH can
// only be produced by moving its start backwards.
func backdateWorktree(t *testing.T, repo, taskID string, ago time.Duration) {
	t.Helper()
	created := time.Now().UTC().Add(-ago)
	idx, err := index.New(filepath.Join(repo, ".mgit", "index.db"),
		func() time.Time { return created })
	require.NoError(t, err)
	defer func() { require.NoError(t, idx.Close()) }()

	require.NoError(t, idx.InsertWorktree(t.Context(), &model.WorktreeInfo{
		Path: filepath.Join(repo, "wt"), Branch: "task/" + taskID,
		TaskID: taskID, AgentID: "claude-" + taskID,
	}))
}

// TestLog_TaskID_EndBurstTrail_LabelsItPackagedPostHoc is the whole point of
// MGIT-110 reaching a reviewer: two commits seconds apart at the end of a
// forty-minute run are shown for what they are, on the very command that
// lists the trail. Refs: MGIT-110, R-H234
func TestLog_TaskID_EndBurstTrail_LabelsItPackagedPostHoc(t *testing.T) {
	const task = "MGIT-110"
	repo := seedCadenceRepo(t, task)
	backdateWorktree(t, repo, task, 40*time.Minute)

	out, err := runCLIOut(t, "log", "--task-id", task)

	require.NoError(t, err)
	assert.Contains(t, out, model.CadencePackagedPostHoc,
		"the reviewer reading the trail must see the label attached to it")
	// The listing is still there, and now says what each commit WAS. It used to
	// assert "pos=0", which is what a reviewer never came for (MGIT-155).
	assert.Contains(t, out, "step one", "the commit listing is still there, with subjects")
	assert.Contains(t, out, "step two")
	assert.NotContains(t, out, "pos=", "the index position is not the reviewer's business")
}

// TestLog_TaskID_JSON_CarriesTheStableToken pins the machine-readable shape.
// The token is the contract, per the MGIT-109 precedent; a consumer must
// never have to match on the summary prose. Refs: MGIT-110, MGIT-109, R-H234
func TestLog_TaskID_JSON_CarriesTheStableToken(t *testing.T) {
	const task = "MGIT-110"
	repo := seedCadenceRepo(t, task)
	backdateWorktree(t, repo, task, 40*time.Minute)

	out, err := runCLIOut(t, "log", "--task-id", task, "--json")

	require.NoError(t, err)
	var got taskLogJSON
	require.NoError(t, json.Unmarshal([]byte(out), &got))

	assert.Equal(t, task, got.TaskID)
	assert.Len(t, got.Commits, 2, "the commit records still travel")
	require.NotNil(t, got.Cadence)
	assert.Equal(t, model.CadencePackagedPostHoc, got.Cadence.Verdict)
	assert.True(t, model.ValidCadenceVerdict(got.Cadence.Verdict))
	require.NotNil(t, got.Cadence.Measure)
	assert.Equal(t, model.CadenceDenominatorWorktree, got.Cadence.Measure.Denominator,
		"the denominator a verdict rests on is published with it")
	assert.NotEmpty(t, got.Cadence.Summary)
}

// TestLog_TaskID_NoWorktree_RefusesToLabelAtTheSurface: the refusal has to
// reach the reviewer as a refusal. A missing label would read as approval.
// Refs: MGIT-110
func TestLog_TaskID_NoWorktree_RefusesToLabelAtTheSurface(t *testing.T) {
	const task = "MGIT-110"
	seedCadenceRepo(t, task)

	out, err := runCLIOut(t, "log", "--task-id", task, "--json")

	require.NoError(t, err)
	var got taskLogJSON
	require.NoError(t, json.Unmarshal([]byte(out), &got))

	require.NotNil(t, got.Cadence)
	assert.Equal(t, model.CadenceInsufficientEvidence, got.Cadence.Verdict)
	assert.Equal(t, model.CadenceReasonNoDenominator, got.Cadence.Reason)
	assert.Nil(t, got.Cadence.Measure, "no measurement is published without a denominator")

	human, err := runCLIOut(t, "log", "--task-id", task)
	require.NoError(t, err)
	assert.Contains(t, human, model.CadenceInsufficientEvidence)
	assert.Contains(t, human, model.CadenceReasonNoDenominator)
}

// TestLog_TaskID_NoCommits_SaysSoRatherThanPrintingNothing: an interrupted
// agent's task is the MGIT-109 case, and an empty listing is exactly what
// hid it the first time. Refs: MGIT-110, MGIT-109
func TestLog_TaskID_NoCommits_SaysSoRatherThanPrintingNothing(t *testing.T) {
	repo := t.TempDir()
	t.Chdir(repo)
	require.NoError(t, runCLI(t, "init"))
	backdateWorktree(t, repo, "MGIT-109", 30*time.Minute)

	out, err := runCLIOut(t, "log", "--task-id", "MGIT-109")

	require.NoError(t, err)
	assert.Contains(t, out, model.CadenceNoCommits)
}

// TestShortHash_ShorterThanTheAbbreviation_ReturnsItWhole: the listing must
// not panic on a hash shorter than the display width. Refs: MGIT-110
func TestShortHash_ShorterThanTheAbbreviation_ReturnsItWhole(t *testing.T) {
	assert.Equal(t, "abc", shortHash("abc"))
	assert.Equal(t, "abcdef12", shortHash("abcdef1234567890"))
}

// TestLog_HelpDocumentsTheCadenceTokens puts the contract where an integrator
// about to script the command reads it — the same placement MGIT-109 settled
// on for the policy failure codes. Refs: MGIT-110, MGIT-109
func TestLog_HelpDocumentsTheCadenceTokens(t *testing.T) {
	out, err := runCLIOut(t, "log", "--help")

	require.NoError(t, err)
	for _, token := range append(model.CadenceVerdicts(), model.CadenceReasons()...) {
		assert.Contains(t, out, token, "help must document every token it can emit")
	}
}
