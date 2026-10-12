//go:build !linux || (libkrun && cgo)

package main

import (
	"log/slog"
	"time"

	"github.com/hyper-swe/mgit/internal/service"
	"github.com/hyper-swe/mgit/internal/store/index"
)

// wireEgress is a no-op off the Linux firecracker build, and on the libkrun
// build wherever it runs. The allowlist host-tap proxy/DNS enforcement is the
// firecracker (KVM) backend's. The libkrun backend enforces egress inside its
// own VM child and creates no host tap, so installing the firecracker
// controller there bound a gateway address that does not exist and failed every
// allowlist sandbox at boot (MGIT-287); its live policy verbs are served by the
// libkrun path (selectPolicyController). The macOS vzf backend's
// allowlist support is tracked separately (it uses a NAT attachment, not a
// host tap + firewall). With no host egress runner there is no live granter,
// so capability escalation's egress-widening is Linux-only too; the service
// runs without an egress controller or capability revoker (both nil-safe).
// Refs: FR-17.7, FR-17.12
func wireEgress(_ *service.SandboxService, _ *index.Store, _ func() time.Time, _ *slog.Logger) egressWiring {
	return egressWiring{}
}
