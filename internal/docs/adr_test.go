package docs

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

// TestADR_005_Accepted verifies ADR-005 (microVM sandbox) is promoted
// to Accepted with a dated revision history, and that the SEC-02
// remediation is consistent (no enforcement config inside the repo
// worktree). Refs: FR-17, MGIT-11.1.3
func TestADR_005_Accepted(t *testing.T) {
	adr := readRepoFile(t, "docs", "adr", "005-microvm-sandbox.md")

	assert.Contains(t, adr, "**Status:** Accepted", "ADR-005 must be Accepted")
	assert.Contains(t, adr, "## Revision History", "ADR-005 must carry a revision history")
	assert.Contains(t, adr, "2026-06-12", "revision history must be dated")
	assert.NotContains(t, adr, ".mgit/sandbox/policy.json",
		"enforcement config must not live in the worktree (SEC-02)")
	assert.NotContains(t, adr, ".mgit/sandbox/images.lock",
		"image lock must not live in the worktree (SEC-02)")
	assert.NotContains(t, adr, "MGIT-9.",
		"task references must point at the imported MGIT-11 epic, not the stale MGIT-9 numbering")
}

// TestADR_004_RefsSEC03 verifies ADR-004 cross-references the SEC-03
// resolution: worktree object-store sharing must not extend into
// sandbox guests. Refs: FR-17.4, MGIT-11.1.3
func TestADR_004_RefsSEC03(t *testing.T) {
	adr := readRepoFile(t, "docs", "adr", "004-pluggable-worktree.md")

	assert.Contains(t, adr, "SEC-03", "ADR-004 must reference the SEC-03 finding")
	assert.Contains(t, adr, "ADR-005", "ADR-004 must cross-reference ADR-005")
}

// TestADR_003_ToolQualForSandbox verifies ADR-003 states the DO-330
// tool-qualification position for the sandbox components (F-04).
// Refs: FR-17.30, MGIT-11.1.3
func TestADR_003_ToolQualForSandbox(t *testing.T) {
	adr := readRepoFile(t, "docs", "adr", "003-do178c-scope.md")

	assert.Contains(t, adr, "mgit-sandboxd",
		"ADR-003 must state the DO-330 position for the host helper daemon")
	assert.Contains(t, adr, "mgit-guest",
		"ADR-003 must state the position for the guest agent (untrusted by design)")
	assert.Contains(t, adr, "COTS",
		"ADR-003 must state the COTS assessment position for the VMM")
	assert.Contains(t, adr, "ADR-005",
		"ADR-003 must cross-reference ADR-005")
}
