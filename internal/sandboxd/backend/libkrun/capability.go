package libkrun

import (
	"fmt"

	"github.com/hyper-swe/mgit/internal/model"
)

// netCapabilityProbe reports whether the LINKED libkrun can attach a network
// device. It is a seam so the fail-closed path is testable: we cannot ship a
// deliberately-broken libkrun to test against.
//
// The real implementation lives in the CGO binding and asks the dynamic
// loader directly, rather than creating a libkrun context — which keeps the
// probe entirely outside the single-call-site funnel that guarantees every
// context gets a NIC (enforcement_test.go). Refs: MGIT-61.14
type netCapabilityProbe interface {
	// ProbeNetworking returns nil when the linked libkrun exposes the
	// net-device API, and an error naming what is missing otherwise.
	ProbeNetworking() error
}

// requireNetworking refuses to proceed against a libkrun that cannot attach a
// network device.
//
// WHY THIS IS FAIL-CLOSED WITH NO FALLBACK: mgit attaches an explicit NIC in
// EVERY network mode, including "none". That is a security requirement, not a
// feature — libkrun enables TSI (Transparent Socket Impersonation) when a VM
// has no net device, which proxies the guest's sockets through the host and
// hands it full egress (ADR-010, measured). So "run without a NIC" is not a
// degraded mode, it IS the leak, and there is nothing to degrade to.
//
// libkrun gates those symbols behind an opt-in build flag, so an otherwise
// healthy-looking install can be missing them. A nil probe means this build
// has no binding at all; newPlatformAPI already refuses that with its own
// actionable message, and duplicating it here would only produce a second,
// more confusing failure. Refs: MGIT-61.14, ADR-010, SEC-04
func requireNetworking(probe netCapabilityProbe) error {
	if probe == nil {
		return nil
	}
	if err := probe.ProbeNetworking(); err != nil {
		return fmt.Errorf(
			"%w: the linked libkrun was built WITHOUT networking support (%w). "+
				"mgit attaches an explicit network device to every sandbox in every "+
				"mode — without one libkrun falls back to TSI and the guest gets full "+
				"host egress — so there is no safe way to continue. "+
				"Rebuild libkrun with networking enabled (upstream: `make NET=1`), or "+
				"install a libkrun package that enables it. Verify with: "+
				"nm -gU $(brew --prefix libkrun)/lib/libkrun.dylib | grep krun_add_net_unixgram",
			model.ErrSandboxBackendUnavailable, err)
	}
	return nil
}
