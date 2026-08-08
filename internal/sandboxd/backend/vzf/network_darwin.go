//go:build darwin && cgo

package vzf

import (
	"fmt"

	"github.com/hyper-swe/mgit/internal/model"
)

// refuseUnenforceableNetwork rejects a network mode this backend cannot
// actually enforce, rather than approximating it.
//
// vzf attaches a macOS NAT device (vz.NewNATNetworkDeviceAttachment) whenever
// the mode is not "none". NAT is full egress: there is no host tap, no
// firewall, no egress proxy and no pinning resolver on this backend — and
// wireEgress is a no-op off Linux, so nothing else supplies them either. So
// `--network allowlist` on vzf gave the guest an UNRESTRICTED network while
// naming a policy, which is the SEC-04 false-containment the audit rejected:
// the user believes destinations are being filtered and none are.
//
// Refusing is the same call the container backend already makes for the same
// reason (its networkArg: "allowlist mode is refused, not approximated").
// Honest-blocked beats dishonest-allowed — a launch that fails naming the
// limitation is recoverable; a sandbox that silently ignores its policy is
// not. Refs: MGIT-70, SEC-04, FR-17.7, FR-17.8
func refuseUnenforceableNetwork(mode string) error {
	if mode != model.NetworkModeAllowlist {
		return nil // none has no NIC; open is honestly unrestricted by definition
	}
	return fmt.Errorf(
		"%w: allowlist mode is not enforceable on the vzf backend — it attaches a macOS "+
			"NAT device with no host tap, firewall, egress proxy or pinning resolver, so "+
			"the guest would get UNRESTRICTED egress under an allowlist policy. Use the "+
			"default libkrun backend on macOS (its netstack gateway enforces allowlist "+
			"mode), or --network none",
		model.ErrNetworkPolicyViolation)
}
