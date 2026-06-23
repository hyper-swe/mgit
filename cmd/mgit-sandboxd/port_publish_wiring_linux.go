//go:build linux

package main

import (
	"log/slog"

	"github.com/hyper-swe/mgit/internal/sandboxd/backend/firecracker"
	"github.com/hyper-swe/mgit/internal/sandboxd/portpub"
	"github.com/hyper-swe/mgit/internal/service"
)

// wirePortPublish installs the one-way port-publish controller on the sandbox
// service (Linux/KVM, SEC-09). The controller binds a 127.0.0.1 host listener
// per published port and forwards into the guest over the firecracker per-VM
// vsock port dialer (stateless host I/O reconstructed from workDir + sandbox
// ID, exactly like the land dialer), tearing every listener down on teardown
// (no residue, FR-17.19). The host->guest direction ONLY: the guest gets no
// path back to a host loopback service. A wiring failure is logged and leaves
// port publishing disabled (the service is nil-safe), never half-wired.
// Refs: SEC-09, FR-17.8, FR-17.19
func wirePortPublish(svc *service.SandboxService, workDir string, logger *slog.Logger) {
	ctrl, err := portpub.New(portpub.Config{
		Dialer: firecracker.NewPortDialer(workDir),
		Logger: logger,
	})
	if err != nil {
		logger.Error("sandbox port-publish wiring failed; publishing disabled", "error", err.Error())
		return
	}
	svc.SetPortPublishController(ctrl)
	logger.Info("sandbox port publishing wired", "event", "port_publish_wired")
}
