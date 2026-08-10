package main

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/hyper-swe/mgit/internal/model"
)

// TestSandboxSync_Delivers_ReportsWhatItDid is the POSITIVE CONTROL for the
// refusal tests below: a sync that really propagates says exactly what moved,
// so a refusal is distinguishable from a broken path. Refs: MGIT-76
func TestSandboxSync_Delivers_ReportsWhatItDid(t *testing.T) {
	c := &fakeSandboxClient{syncReport: &model.WorktreeSyncReport{
		Updated: []string{"src/app.go", "README.md"}, Deleted: []string{"old.txt"},
	}}

	out, err := runSandbox(okConnect(c), "sync", "--task", "MGIT-76")

	require.NoError(t, err)
	assert.Equal(t, "MGIT-76", c.syncTask)
	assert.Equal(t, model.WorktreeSyncOptions{}, c.syncOpts)
	assert.Contains(t, out, "2 updated")
	assert.Contains(t, out, "1 deleted")
	assert.Contains(t, out, "src/app.go")
	assert.Contains(t, out, "old.txt")
}

// TestSandboxSync_UnchangedWorktree_SaysSoRatherThanReportingPhantomWork
// verifies the cheap no-op is visible as a no-op. Refs: MGIT-76
func TestSandboxSync_UnchangedWorktree_SaysSoRatherThanReportingPhantomWork(t *testing.T) {
	c := &fakeSandboxClient{syncReport: &model.WorktreeSyncReport{Skipped: true}}

	out, err := runSandbox(okConnect(c), "sync", "--task", "MGIT-76")

	require.NoError(t, err)
	assert.Contains(t, out, "already up to date")
	assert.NotContains(t, out, "updated")
}

// TestSandboxSync_NotBootedSandbox_ReportsTheReason verifies the honest no-op
// carries its explanation instead of looking like a completed sync.
func TestSandboxSync_NotBootedSandbox_ReportsTheReason(t *testing.T) {
	c := &fakeSandboxClient{syncReport: &model.WorktreeSyncReport{
		Skipped: true, Detail: "the sandbox has not booted yet",
	}}

	out, err := runSandbox(okConnect(c), "sync", "--task", "MGIT-76")

	require.NoError(t, err)
	assert.Contains(t, out, "has not booted yet")
}

// TestSandboxSync_DryRun_ReportsTheClassification verifies --dry-run projects
// rather than delivers, and says which it did. Refs: MGIT-76
func TestSandboxSync_DryRun_ReportsTheClassification(t *testing.T) {
	c := &fakeSandboxClient{syncReport: &model.WorktreeSyncReport{
		DryRun: true, Updated: []string{"src/app.go"},
	}}

	out, err := runSandbox(okConnect(c), "sync", "--task", "MGIT-76", "--dry-run")

	require.NoError(t, err)
	assert.True(t, c.syncOpts.DryRun)
	assert.Contains(t, out, "would")
	assert.Contains(t, out, "src/app.go")
	assert.NotContains(t, strings.ToLower(out), "synced host worktree")
}

// TestSandboxSync_DryRun_NamesEveryConflictAndItsReason is the report HyperSwe
// cannot obtain today without running a command in the guest and being
// refused. Refs: MGIT-76
func TestSandboxSync_DryRun_NamesEveryConflictAndItsReason(t *testing.T) {
	c := &fakeSandboxClient{syncReport: &model.WorktreeSyncReport{
		DryRun: true, Refused: true,
		Updated: []string{"doc.md"},
		Conflicts: []model.WorktreeSyncConflict{
			{Path: "src/app.go", Reason: "modified in the guest since it was delivered"},
			{Path: "gen.go", Reason: "created in the guest; the host now has a file of the same name"},
		},
	}}

	out, err := runSandbox(okConnect(c), "sync", "--task", "MGIT-76", "--dry-run")

	require.NoError(t, err, "a dry run reports; it does not refuse")
	assert.Contains(t, out, "would be REFUSED")
	assert.Contains(t, out, "src/app.go")
	assert.Contains(t, out, "modified in the guest")
	assert.Contains(t, out, "gen.go")
	assert.Contains(t, out, "created in the guest")
	assert.Contains(t, out, "doc.md", "the paths that would move are shown too")
}

// TestSandboxSync_Conflict_IsRefusedNamingEveryPath verifies a real sync
// refuses with a non-zero exit and names what blocked it. Refs: MGIT-76
func TestSandboxSync_Conflict_IsRefusedNamingEveryPath(t *testing.T) {
	c := &fakeSandboxClient{
		syncReport: &model.WorktreeSyncReport{Refused: true,
			Conflicts: []model.WorktreeSyncConflict{{Path: "src/app.go", Reason: "modified in the guest"}}},
		syncErr: model.ErrWorktreeSyncConflict,
	}

	out, err := runSandbox(okConnect(c), "sync", "--task", "MGIT-76")

	require.Error(t, err, "a refusal must be a non-zero exit, not a printed note")
	assert.Contains(t, out, "src/app.go")
	assert.Contains(t, out, "modified in the guest")
	assert.Contains(t, out, "--force", "the refusal names a remedy")
}

// TestSandboxSync_Force_ReportsEveryDestroyedPath verifies --force is
// available but never silent. Refs: MGIT-76
func TestSandboxSync_Force_ReportsEveryDestroyedPath(t *testing.T) {
	c := &fakeSandboxClient{syncReport: &model.WorktreeSyncReport{
		Updated: []string{"src/app.go"}, Overridden: []string{"src/app.go"},
	}}

	out, err := runSandbox(okConnect(c), "sync", "--task", "MGIT-76", "--force")

	require.NoError(t, err)
	assert.True(t, c.syncOpts.Force)
	assert.Contains(t, out, "overwritten")
	assert.Contains(t, out, "src/app.go")
}

// TestSandboxSync_UnsupportedBackend_FailsClosedNamingTheLimitation verifies
// the firecracker case surfaces as a refusal at the CLI, not a success. A sync
// that claims to have run is how stale code gets executed. Refs: MGIT-76
func TestSandboxSync_UnsupportedBackend_FailsClosedNamingTheLimitation(t *testing.T) {
	c := &fakeSandboxClient{syncErr: model.ErrSandboxSyncUnsupported}

	out, err := runSandbox(okConnect(c), "sync", "--task", "MGIT-76")

	require.Error(t, err)
	assert.NotContains(t, out, "up to date")
	assert.NotContains(t, out, "Synced")
}

// TestSandboxSync_JSON_EmitsTheWholeReport verifies an agent gets the
// classification as data, including on a refusal. Refs: MGIT-76
func TestSandboxSync_JSON_EmitsTheWholeReport(t *testing.T) {
	c := &fakeSandboxClient{syncReport: &model.WorktreeSyncReport{
		DryRun: true, Refused: true, Updated: []string{"doc.md"},
		Conflicts: []model.WorktreeSyncConflict{{Path: "src/app.go", Reason: "modified in the guest"}},
	}}

	out, err := runSandbox(okConnect(c), "sync", "--task", "MGIT-76", "--dry-run", "--json")

	require.NoError(t, err)
	var got model.WorktreeSyncReport
	require.NoError(t, json.Unmarshal([]byte(out), &got))
	assert.True(t, got.DryRun)
	assert.True(t, got.Refused)
	assert.Equal(t, []string{"doc.md"}, got.Updated)
	require.Len(t, got.Conflicts, 1)
	assert.Equal(t, "src/app.go", got.Conflicts[0].Path)
}

// TestSandboxSync_MissingTask_IsRejected verifies the verb refuses to guess
// which sandbox it means.
func TestSandboxSync_MissingTask_IsRejected(t *testing.T) {
	c := &fakeSandboxClient{}

	_, err := runSandbox(okConnect(c), "sync")

	require.Error(t, err)
	assert.Empty(t, c.syncTask)
}

// TestSandboxSync_DaemonUnavailable_FailsClosed verifies there is no local
// fallback: an unreachable daemon can never look like a completed sync.
func TestSandboxSync_DaemonUnavailable_FailsClosed(t *testing.T) {
	failConnect := func(context.Context) (sandboxClient, error) { return nil, assert.AnError }

	out, err := runSandbox(failConnect, "sync", "--task", "MGIT-76")

	require.Error(t, err)
	assert.NotContains(t, out, "up to date")
}

// TestSandboxSync_Force_DoesNotAdviseForcingAgain verifies a sync that already
// forced its way through reports each destroyed path WITH the guest change it
// destroyed, and does not print the "re-run with --force" remedy — advice to
// repeat what just happened, on a command that succeeded. Refs: MGIT-76
func TestSandboxSync_Force_DoesNotAdviseForcingAgain(t *testing.T) {
	c := &fakeSandboxClient{syncReport: &model.WorktreeSyncReport{
		Updated: []string{"src/app.go"}, Overridden: []string{"src/app.go"},
		Conflicts: []model.WorktreeSyncConflict{
			{Path: "src/app.go", Reason: "modified in the guest since it was delivered"},
		},
	}}

	out, err := runSandbox(okConnect(c), "sync", "--task", "MGIT-76", "--force")

	require.NoError(t, err)
	assert.Contains(t, out, "overwritten src/app.go (modified in the guest since it was delivered)")
	assert.NotContains(t, out, "re-run with --force",
		"a successful forced sync must not advise forcing again")
	assert.NotContains(t, out, "conflict    src/app.go",
		"an overwritten path no longer blocks anything")
}
