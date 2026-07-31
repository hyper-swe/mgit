//go:build libkrun && cgo && (darwin || linux)

package main

import (
	"log/slog"

	"github.com/hyper-swe/mgit/internal/sandboxd/backend/libkrun"
	"github.com/hyper-swe/mgit/internal/sandboxd/portpub"
	"github.com/hyper-swe/mgit/internal/service"
)

// wirePortPublish installs the one-way port-publish controller for the
// libkrun backend (SEC-09). It forwards into the guest over libkrun's own
// LISTENING vsock ports — the VMM listens on a per-port host unix socket and
// forwards inbound connections to the guest, where mgit-guest bridges them to
// the guest's loopback.
//
// It deliberately does NOT use the netstack gateway's DialGuestPort: that
// gateway lives in the VM child process, so the daemon holds no reference to
// it. libkrun's vsock ports cross the process boundary by construction, which
// is also the shape firecracker already uses — so the publisher and the guest
// bridge are reused unchanged. Host->guest only; no path back.
// Refs: SEC-09, FR-17.8, FR-17.19, MGIT-61.13 P6
func wirePortPublish(svc *service.SandboxService, workDir string, logger *slog.Logger) {
	ctrl, err := portpub.New(portpub.Config{
		Dialer: libkrun.NewPortDialer(workDir),
		Logger: logger,
	})
	if err != nil {
		logger.Error("sandbox port-publish wiring failed; publishing disabled", "error", err.Error())
		return
	}
	svc.SetPortPublishController(ctrl)
	logger.Info("sandbox port publishing wired", "event", "port_publish_wired", "backend", "libkrun")
}
