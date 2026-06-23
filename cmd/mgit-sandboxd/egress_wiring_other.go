//go:build !linux

package main

import (
	"log/slog"
	"time"

	"github.com/hyper-swe/mgit/internal/service"
	"github.com/hyper-swe/mgit/internal/store/index"
)

// wireEgress is a no-op off Linux. The allowlist host-tap proxy/DNS
// enforcement is the firecracker (KVM) backend's; the macOS vzf backend's
// allowlist support is tracked separately (it uses a NAT attachment, not a
// host tap + firewall). With no host egress runner there is no live granter,
// so capability escalation's egress-widening is Linux-only too; the service
// runs without an egress controller or capability revoker (both nil-safe).
// Refs: FR-17.7, FR-17.12
func wireEgress(_ *service.SandboxService, _ *index.Store, _ func() time.Time, _ *slog.Logger) *service.CapabilityService {
	return nil
}
