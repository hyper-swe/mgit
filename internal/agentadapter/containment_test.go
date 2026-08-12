package agentadapter

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
)

// TestRenderClaudeMd_Active_IsDefault confirms the zero-value posture renders
// the "commands run in a microVM" block (backward-compatible default used by
// the live-sandbox regeneration path). Refs: MGIT-47
func TestRenderClaudeMd_Active_IsDefault(t *testing.T) {
	s := RenderClaudeMdSection(SandboxEnv{WorktreePath: "/repo/wt", NetworkMode: "none"})
	assert.Contains(t, s, "microVM")
	assert.Contains(t, s, "routes through `mgit run`")
}

// TestRenderClaudeMd_Open_IsHonestOpen is the core MGIT-47 fix: on a machine
// with no sandbox, the block must NOT claim routing is active or that commands
// run in a microVM. It must tell the agent to run commands normally on the
// host and point at how to enable containment. Refs: MGIT-47
func TestRenderClaudeMd_Open_IsHonestOpen(t *testing.T) {
	s := RenderClaudeMdSection(SandboxEnv{WorktreePath: "/repo/wt", NetworkMode: "none", Containment: ContainmentOpen})

	// Must NOT claim the sandbox is active.
	assert.NotContains(t, s, "routes through `mgit run`",
		"open posture must not claim shell routing is active")
	assert.NotContains(t, s, "hardware-isolated **microVM**",
		"open posture must not claim commands run in a microVM")

	// Must be honest that commands run on the host and containment is off.
	low := strings.ToLower(s)
	assert.Contains(t, low, "no sandbox")
	assert.Contains(t, low, "host")
	assert.Contains(t, s, "mgit-sandboxd", "must point at how to enable containment")

	// The sandbox-agnostic working discipline still applies.
	assert.Contains(t, s, "mgit commit")
}

// TestRenderClaudeMd_Pending_FailsClosed: when a sandbox was requested but is
// not running, the block must say so and keep the fail-closed contract (never
// claim commands run normally on the host). Refs: MGIT-47
func TestRenderClaudeMd_Pending_FailsClosed(t *testing.T) {
	s := RenderClaudeMdSection(SandboxEnv{WorktreePath: "/repo/wt", NetworkMode: "none", Containment: ContainmentPending})
	low := strings.ToLower(s)
	assert.Contains(t, low, "request")
	assert.Contains(t, low, "fail")                        // fail-closed
	assert.Contains(t, s, "mgit sandbox launch")           // how to bring it up
	assert.NotContains(t, low, "run commands normally — ") // must not invite host execution
}

// TestContainmentStatusLine gives a single machine-parseable line per posture,
// prefixed "Containment:", for `mgit work` output. Refs: MGIT-47
func TestContainmentStatusLine(t *testing.T) {
	tests := []struct {
		c        Containment
		contains string
	}{
		{ContainmentActive, "active"},
		{ContainmentPending, "requested"},
		{ContainmentOpen, "none"},
	}
	for _, tt := range tests {
		line := ContainmentStatusLine(tt.c)
		assert.True(t, strings.HasPrefix(line, "Containment: "), "line %q lacks parseable prefix", line)
		assert.Contains(t, line, tt.contains)
	}
}

// TestContainmentOpen_StatesOnlyWhatWasProbed is MGIT-102's loose thread: the
// Open posture is selected purely because `--sandbox` was NOT passed — nothing
// anywhere probes whether a sandbox backend exists on this host. The old
// wording nonetheless asserted "no sandbox on this host" and told the reader to
// "install mgit-sandboxd", which was measurably wrong on a machine where
// mgit-sandboxd was installed and on PATH.
//
// That matters here specifically: this ticket is about containment status being
// TRUSTWORTHY. A status line that asserts an unprobed host fact sends a user to
// install something they already have, and implies containment is impossible
// where it is one flag away. State what is known — nothing was requested.
// Refs: MGIT-47, MGIT-102
func TestContainmentOpen_StatesOnlyWhatWasProbed(t *testing.T) {
	line := ContainmentStatusLine(ContainmentOpen)
	body := RenderClaudeMdSection(SandboxEnv{WorktreePath: "/repo/wt", NetworkMode: "none", Containment: ContainmentOpen})

	for _, unprobed := range []string{
		"no sandbox on this host",
		"no sandbox on this machine",
		"No sandbox is active on this machine",
		"containment is unavailable",
	} {
		assert.NotContains(t, line, unprobed, "the status line asserts a host fact nothing probed")
		assert.NotContains(t, body, unprobed, "the CLAUDE.md block asserts a host fact nothing probed")
	}

	// What IS known: containment was not requested for this worktree, and how
	// to request it.
	assert.Contains(t, strings.ToLower(line), "no sandbox was requested for this worktree",
		"the line must say what was actually established")
	assert.Contains(t, line, "--sandbox", "and how to get containment")
	assert.Contains(t, strings.ToLower(body), "no sandbox was requested for this worktree")
	assert.Contains(t, body, "mgit work --sandbox")

	// The honest-open contract (MGIT-47) is unchanged: commands run on the
	// host, and nothing claims routing.
	assert.Contains(t, strings.ToLower(body), "host")
	assert.NotContains(t, body, "routes through `mgit run`")
}
