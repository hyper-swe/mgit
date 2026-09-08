package packaging

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

// The firecracker live gate asserts its named proofs POSITIVELY — a run in
// which one of them did not PASS fails the job — because their prerequisites
// (KVM, root, a guest image) skip silently. The staged-policy proof
// (MGIT-156: a policy staged onto an unbooted sandbox is enforced by the
// guest that boots) joins the MGIT-78 pair in that list, so dropping the
// test, renaming it, or letting it skip is a red job, not a quiet gap.
// Refs: MGIT-156, MGIT-109, MGIT-78
func TestE2E_NamesTheStagedPolicyProofAmongThePositivelyAssertedTests(t *testing.T) {
	cfg := readRepoFile(t, ".github/workflows/e2e.yml")
	assert.Contains(t, cfg, "TestE2E_Firecracker_StagedPolicy_CreatedSandboxBootsEnforcingIt",
		"e2e.yml must name the MGIT-156 proof in the positively-asserted list of the root-gated firecracker half")
}
