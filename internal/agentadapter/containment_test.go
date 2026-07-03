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
