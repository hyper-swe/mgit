package packaging

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

// The unprivileged guest identity is proven live in TWO places, each
// asserted by name so a skip or a rename is a red job: the firecracker
// unprivileged half (the daemon's identity is the runner user, the guest
// must switch to it and write the worktree as it) and the root-gated half
// (the supervisor's credential switch, no VM). Refs: MGIT-151
func TestE2E_NamesTheGuestIdentityProofs(t *testing.T) {
	cfg := readRepoFile(t, ".github/workflows/e2e.yml")
	assert.Contains(t, cfg, "--- PASS: TestE2E_Exec_RunsAsTheIdentityAsked",
		"the unprivileged half asserts the firecracker identity proof by name")
	assert.Contains(t, cfg, "TestExecute_AsRoot_SwitchesToTheAskedIdentity",
		"the root half runs the supervisor's credential-switch proof")
}
