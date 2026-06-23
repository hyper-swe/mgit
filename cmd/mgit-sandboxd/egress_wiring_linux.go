//go:build linux

package main

import (
	"context"
	"log/slog"
	"net"
	"net/netip"
	"time"

	"github.com/hyper-swe/mgit/internal/model"
	"github.com/hyper-swe/mgit/internal/sandboxd/backend/firecracker"
	"github.com/hyper-swe/mgit/internal/sandboxd/egress"
	"github.com/hyper-swe/mgit/internal/service"
	"github.com/hyper-swe/mgit/internal/store/index"
)

// Fixed gateway ports the egress proxy and DNS server bind per sandbox — the
// same ports the firecracker tap firewall steers the guest to. Refs: SEC-04
const (
	egressProxyPort = 1080
	egressDNSPort   = 53
)

// wireEgress installs the host egress controller on the sandbox service
// (Linux/KVM). For an allowlist sandbox the service then starts the proxy +
// restricted DNS on the firecracker tap gateway at boot and stops them at
// teardown; none/open sandboxes run no proxy. Wiring is unconditional on
// Linux: the container fallback refuses allowlist mode before launch, and
// none/open are no-ops in the runner, so no spurious listeners are bound.
// It also wires capability escalation: the same egress.Runner is the live
// allowlist-widening granter, and the CapabilityService is installed as the
// sandbox service's grant revoker so grants die with the sandbox (SEC-05).
// Refs: FR-17.7, FR-17.8, FR-17.12, SEC-04, SEC-05
func wireEgress(svc *service.SandboxService, events *index.Store, clock func() time.Time, logger *slog.Logger) {
	runner, err := egress.NewRunner(egress.RunnerConfig{
		Audit:     events,
		Lookup:    egress.SystemLookup(nil),
		Dial:      hostEgressDial,
		Clock:     clock,
		Logger:    logger,
		ProxyPort: egressProxyPort,
		DNSPort:   egressDNSPort,
	})
	if err != nil {
		logger.Error("sandbox egress wiring failed; allowlist mode will fail closed", "error", err.Error())
		return
	}
	svc.SetEgressController(fcEgressController{runner: runner})

	// Capability escalation: a host-observed egress denial can be escalated to a
	// scoped, audited grant that widens THIS runner's live allowlist; the grant
	// is revoked when the sandbox tears down. Refs: FR-17.12, SEC-05
	capSvc, err := service.NewCapabilityService(events, runner, clock)
	if err != nil {
		logger.Error("capability escalation wiring failed; escalation disabled", "error", err.Error())
	} else {
		svc.SetCapabilityRevoker(capSvc)
	}
	logger.Info("sandbox egress enforcement wired", "event", "egress_wired",
		"proxy_port", egressProxyPort, "dns_port", egressDNSPort)
}

// fcEgressController adapts egress.Runner to service.EgressController,
// resolving the firecracker per-sandbox tap gateway the proxy/DNS bind.
type fcEgressController struct{ runner *egress.Runner }

// StartEgress brings up the sandbox's egress stack on its tap gateway.
func (c fcEgressController) StartEgress(ctx context.Context, info model.SandboxInfo) error {
	_, err := c.runner.Start(ctx, egress.Binding{
		SandboxID: info.ID,
		TaskID:    info.TaskID,
		GatewayIP: firecracker.GatewayFor(info.ID),
		Policy:    model.NetworkPolicy{Mode: info.NetworkMode, Allowlist: info.NetworkAllowlist},
	})
	return err
}

// StopEgress tears the sandbox's egress stack down (idempotent).
func (c fcEgressController) StopEgress(sandboxID string) { _ = c.runner.Stop(sandboxID) }

// hostEgressDial opens the authorized host-side connection to a destination
// the proxy approved. Refs: SEC-04
func hostEgressDial(ctx context.Context, ip netip.Addr, port int) (net.Conn, error) {
	var d net.Dialer
	return d.DialContext(ctx, "tcp", netip.AddrPortFrom(ip, uint16(port)).String())
}
