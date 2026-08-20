package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/hyper-swe/mgit/internal/agentadapter"
)

// The silence being fixed: provisioning printed one "Containment:" line about
// the GUEST and nothing about whether a given agent could step around it, so an
// advisory lane and an enforced one looked identical. Every family must now be
// named with its tier at provisioning time. Refs: MGIT-149
func TestWorkSetup_Contained_DeclaresRoutingPerFamilyByName(t *testing.T) {
	adder := &fakeWorktreeAdder{}
	path := filepath.Join(t.TempDir(), "wt")
	out, _, err := runWorkSetup(t, adder, workOptions{Path: path, TaskID: "MGIT-149", LaunchSandbox: true}, nil)
	require.NoError(t, err)

	for _, f := range agentadapter.Families() {
		assert.Contains(t, out, f.Display, "provisioning must name the family")
		assert.Contains(t, out, "Routing: "+f.ID+"=",
			"provisioning must emit a machine-parseable tier for %s", f.ID)
	}
	assert.Contains(t, out, "advisory", "the advisory lane must be called that, in plain words")
}

// An open worktree has no routing to report; claiming tiers there would be its
// own overstatement. Refs: MGIT-149, MGIT-47
func TestWorkSetup_Open_DoesNotClaimRoutingItDidNotInstall(t *testing.T) {
	adder := &fakeWorktreeAdder{}
	path := filepath.Join(t.TempDir(), "wt")
	out, _, err := runWorkSetup(t, adder, workOptions{Path: path, TaskID: "MGIT-149b"}, nil)
	require.NoError(t, err)
	assert.NotContains(t, out, "Routing: ", "no routing is installed for an uncontained worktree")
	assert.Contains(t, out, "Containment: none")
}

// The enforced wiring must actually be on disk, not merely announced — the
// report's claims are checkable by opening the files it names. Refs: MGIT-149
func TestWorkSetup_Contained_InstallsTheHookFilesItAdvertises(t *testing.T) {
	adder := &fakeWorktreeAdder{}
	path := filepath.Join(t.TempDir(), "wt")
	_, _, err := runWorkSetup(t, adder, workOptions{Path: path, TaskID: "MGIT-149c", LaunchSandbox: true}, nil)
	require.NoError(t, err)

	for _, f := range agentadapter.Families() {
		if f.Routing == agentadapter.RoutingAdvisory {
			continue
		}
		full := filepath.Join(path, filepath.FromSlash(f.Config))
		b, rerr := os.ReadFile(full) //nolint:gosec // test temp path
		require.NoError(t, rerr, "%s claims enforcement via %s, which must exist", f.Display, f.Config)
		assert.Contains(t, string(b), "sandbox", "%s must wire an mgit hook", f.Config)
	}
}

// --agent names the family that will actually work here, so the operator gets a
// single unmissable verdict instead of a matrix to interpret. Refs: MGIT-149
func TestWorkSetup_AgentFlag_HighlightsThatFamilysTier(t *testing.T) {
	tests := []struct {
		name       string
		agent      string
		wantSubstr string
	}{
		{name: "routed_family_is_stated_as_enforced", agent: "claude", wantSubstr: "Claude Code"},
		{name: "blocked_family_is_stated_as_blocked", agent: "cursor", wantSubstr: "Cursor"},
		{name: "advisory_family_gets_a_loud_warning", agent: "generic", wantSubstr: "ADVISORY"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			adder := &fakeWorktreeAdder{}
			path := filepath.Join(t.TempDir(), "wt")
			out, _, err := runWorkSetup(t, adder,
				workOptions{Path: path, TaskID: "MGIT-149d", LaunchSandbox: true, Agent: tt.agent}, nil)
			require.NoError(t, err)
			assert.Contains(t, out, tt.wantSubstr)
		})
	}
}

func TestWorkSetup_AgentFlag_UnknownFamilyIsRejectedWithTheValidNames(t *testing.T) {
	adder := &fakeWorktreeAdder{}
	path := filepath.Join(t.TempDir(), "wt")
	_, _, err := runWorkSetup(t, adder,
		workOptions{Path: path, TaskID: "MGIT-149e", LaunchSandbox: true, Agent: "nosuchagent"}, nil)
	require.Error(t, err)
	for _, id := range agentadapter.FamilyIDs() {
		assert.Contains(t, err.Error(), id, "the error must list the valid families")
	}
}

// The argued answer to "should --sandbox refuse without enforcement?": NO by
// default, because the advisory lane is the unknown-harness lane and refusing
// it blocks working setups — a refusal that blocks a working lane is its own
// defect. The operator who needs the guarantee asks for it explicitly.
// Refs: MGIT-149
func TestWorkSetup_RequireRouting_RefusesOnlyTheAdvisoryLane(t *testing.T) {
	tests := []struct {
		name    string
		agent   string
		wantErr bool
	}{
		{name: "routed_family_provisions", agent: "claude", wantErr: false},
		{name: "blocked_family_provisions_because_nothing_reaches_the_host", agent: "cursor", wantErr: false},
		{name: "advisory_family_is_refused", agent: "generic", wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			adder := &fakeWorktreeAdder{}
			path := filepath.Join(t.TempDir(), "wt")
			_, _, err := runWorkSetup(t, adder, workOptions{
				Path: path, TaskID: "MGIT-149f", LaunchSandbox: true,
				Agent: tt.agent, RequireRouting: true,
			}, nil)
			if tt.wantErr {
				require.Error(t, err)
				assert.Contains(t, strings.ToLower(err.Error()), "advisory")
				return
			}
			require.NoError(t, err)
		})
	}
}

// --require-routing without --agent cannot know which family will be used, and
// must say so rather than silently passing or silently refusing. Refs: MGIT-149
func TestWorkSetup_RequireRouting_WithoutAgent_IsRejected(t *testing.T) {
	adder := &fakeWorktreeAdder{}
	path := filepath.Join(t.TempDir(), "wt")
	_, _, err := runWorkSetup(t, adder,
		workOptions{Path: path, TaskID: "MGIT-149g", LaunchSandbox: true, RequireRouting: true}, nil)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "--agent")
}
