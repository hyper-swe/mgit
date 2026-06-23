//go:build !linux

package main

import (
	"log/slog"

	"github.com/hyper-swe/mgit/internal/service"
)

// wirePortPublish is a no-op off Linux. v1 wires the one-way port publisher
// (SEC-09) on the firecracker (KVM) backend; the macOS vzf port dialer is
// tracked separately. With no controller installed the service runs without
// port publishing (the collaborator is nil-safe). Refs: SEC-09, FR-17.8
func wirePortPublish(_ *service.SandboxService, _ string, _ *slog.Logger) {}
