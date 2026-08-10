package scaffold

import (
	"os/exec"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// guestBinaries are the packages that build code running INSIDE the sandbox,
// i.e. on the untrusted side of the boundary.
var guestBinaries = []string{"./cmd/mgit-guest"}

// hostOnlyPackages implement or mutate enforcement policy. Nothing that runs
// in the guest may depend on them.
var hostOnlyPackages = []string{
	"github.com/hyper-swe/mgit/internal/sandboxd/egress",
	"github.com/hyper-swe/mgit/internal/sandboxd/worktreesync",
	"github.com/hyper-swe/mgit/internal/sandboxd/staging",
	// The host->child control channel (MGIT-74). A guest that could reach it
	// could ask the child to widen its own policy, which is exactly the
	// property the rest of this file exists to keep true.
	"github.com/hyper-swe/mgit/internal/sandboxd/vmctl",
}

// TestGuestCannotReachPolicyEnforcement proves the SEC-05 authority claim
// structurally rather than by inspection: the guest binary does not LINK the
// packages that decide or mutate egress policy, so there is no code path —
// intended or accidental — by which guest-side code widens its own allowlist.
//
// This matters most for MGIT-72, which makes policy mutable at runtime. A
// runtime-mutable policy is only as trustworthy as the answer to "who may
// mutate it", and "we checked the call sites once" decays. A dependency
// assertion does not: if someone imports the egress package into the guest,
// this fails.
//
// Refs: MGIT-72, SEC-05, FR-17.8
func TestGuestCannotReachPolicyEnforcement(t *testing.T) {
	for _, guestPkg := range guestBinaries {
		t.Run(guestPkg, func(t *testing.T) {
			deps := packageDeps(t, guestPkg)
			for _, forbidden := range hostOnlyPackages {
				assert.NotContains(t, deps, forbidden,
					"%s must not link %s: policy is host-side only, and a guest that "+
						"links the enforcement code is one refactor away from calling it",
					guestPkg, forbidden)
			}
		})
	}
}

// TestPolicyMutationIsNotOnAGuestFacingChannel guards the other direction:
// the wire protocols the guest actually speaks carry no policy verb. The
// guest's channels are exec, land and the egress data path; none of them is a
// control plane for its own containment. Refs: MGIT-72, SEC-05
func TestPolicyMutationIsNotOnAGuestFacingChannel(t *testing.T) {
	for _, wirePkg := range []string{"./internal/execwire", "./internal/landwire", "./internal/guestboot"} {
		t.Run(wirePkg, func(t *testing.T) {
			deps := packageDeps(t, wirePkg)
			for _, forbidden := range hostOnlyPackages {
				assert.NotContains(t, deps, forbidden,
					"the %s wire protocol must not reach %s — a guest-speakable "+
						"protocol that can touch policy is a guest-mutable policy",
					wirePkg, forbidden)
			}
		})
	}
}

// packageDeps returns the full transitive dependency list of a package, as the
// Go toolchain resolves it for a real build.
func packageDeps(t *testing.T, pkg string) []string {
	t.Helper()
	cmd := exec.Command("go", "list", "-deps", pkg) //nolint:gosec // fixed argv over repo-local packages
	cmd.Dir = projectRoot(t)
	out, err := cmd.CombinedOutput()
	require.NoError(t, err, "go list -deps %s: %s", pkg, out)
	return strings.Split(strings.TrimSpace(string(out)), "\n")
}

// TestGuestAuthorityGuardCanActuallyDetectLinkage is the positive control for
// the two guards above.
//
// A dependency assertion that can never fire is worth nothing — it would pass
// just as happily if `go list` returned an empty list or a package name in
// hostOnlyPackages were misspelled or renamed out from under it. This asserts
// the HOST daemon DOES link EVERY package the guest must not, proving the
// check distinguishes linked from not-linked rather than merely never finding
// anything.
//
// It covers the whole forbidden list on purpose. A control that named only one
// package would leave the others' guards vacuous the moment one was renamed —
// which is precisely the decay a structural assertion exists to prevent.
// Refs: MGIT-72, SEC-05
func TestGuestAuthorityGuardCanActuallyDetectLinkage(t *testing.T) {
	deps := packageDeps(t, "./cmd/mgit-sandboxd")

	for _, hostOnly := range hostOnlyPackages {
		assert.Contains(t, deps, hostOnly,
			"the host daemon must link %s; if it does not, the guest-side guard for "+
				"that package is vacuous and proves nothing", hostOnly)
	}
}
