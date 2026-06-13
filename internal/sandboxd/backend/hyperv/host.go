package hyperv

import (
	"fmt"

	"github.com/hyper-swe/mgit/internal/model"
	"github.com/hyper-swe/mgit/internal/sandboxd/backend/microvm"
)

// newPlatformHost returns the platform HCS host. It fails closed
// pending an unresolved design decision (MGIT-11.5.3): running a Linux
// microVM under Hyper-V requires the LCOW utility-VM lifecycle, which
// Microsoft/hcsshim exposes only under internal/uvm + internal/lcow
// (not importable from this module). The approved hcsshim public API
// (CreateContainer / HNS / layers) drives Windows containers, not a
// Linux guest. The host VMM mechanism — vendored LCOW, Cloud
// Hypervisor on WHP, or another path — is pending a decision before
// the real host can be wired; until then a nil Config.Host yields
// ErrSandboxBackendUnavailable rather than a fabricated implementation.
// Refs: FR-17.15, MGIT-11.5.3
func newPlatformHost() (microvm.Hypervisor, error) {
	return nil, fmt.Errorf(
		"%w: Hyper-V Linux-guest host not yet wired — VMM mechanism pending (MGIT-11.5.3); "+
			"hcsshim public API does not expose LCOW utility-VM lifecycle",
		model.ErrSandboxBackendUnavailable)
}
