//go:build !linux

package main

import (
	"context"
	"fmt"
	"log/slog"

	"github.com/hyper-swe/mgit/internal/guest"
)

// serveGuest refuses on non-Linux: mgit-guest is PID 1 inside the
// Linux microVM and has no meaning on the host platform. The binary
// builds everywhere (so the supervisor library compiles and tests on
// any dev machine), but only runs in the guest. Refs: FR-17.16
func serveGuest(_ context.Context, _ *guest.Supervisor, _, _ uint32, _ *slog.Logger) error {
	return fmt.Errorf("mgit-guest runs only as PID 1 inside the Linux microVM guest")
}
