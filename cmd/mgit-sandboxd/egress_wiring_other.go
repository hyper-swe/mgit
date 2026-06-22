//go:build !linux

package main

import (
	"log/slog"
	"time"

	"github.com/hyper-swe/mgit/internal/sandboxd/egress"
	"github.com/hyper-swe/mgit/internal/service"
)

// wireEgress is a no-op off Linux. The allowlist host-tap proxy/DNS
// enforcement is the firecracker (KVM) backend's; the macOS vzf backend's
// allowlist support is tracked separately (it uses a NAT attachment, not a
// host tap + firewall). The service simply runs without an egress
// controller, and non-Linux backends do not offer allowlist mode. Refs: FR-17.7
func wireEgress(_ *service.SandboxService, _ egress.Auditor, _ func() time.Time, _ *slog.Logger) {}
