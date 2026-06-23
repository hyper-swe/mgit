// Command mgit-guest is the minimal PID-1 supervisor baked into
// sandbox rootfs images (ADR-005 Guest Image Discipline, FR-17.16): it
// mounts the worktree and a tmpfs, then serves exec requests over
// vsock with a clean environment. It is PURE TRANSPORT for the trust
// boundary — it holds no signing key and cannot mint attestations
// (SEC-01). The guest runs Linux; on other platforms this binary
// builds but refuses to run. Refs: FR-17.16, SEC-01, MGIT-11.5.6
package main

import (
	"context"
	"flag"
	"io"
	"log/slog"
	"os"
	"os/signal"
	"syscall"

	"github.com/hyper-swe/mgit/internal/guest"
)

func main() {
	os.Exit(run(os.Args[1:], os.Stdout))
}

// run wires flags and delegates to the platform guest loop. Split from
// main for testability (DI; no globals).
func run(args []string, logSink io.Writer) int {
	flags := flag.NewFlagSet("mgit-guest", flag.ContinueOnError)
	flags.SetOutput(logSink)
	port := flags.Uint("vsock-port", 1024, "vsock port to serve exec requests on")
	landPort := flags.Uint("land-vsock-port", 1025, "vsock port to serve the land object pool on")
	notifyPort := flags.Uint("notify-host-port", 1026,
		"host vsock port to signal land-ready on (the auto-land trigger; 0 disables). The guest dials the host; the notification carries no data")
	if err := flags.Parse(args); err != nil {
		return 2
	}

	logger := slog.New(slog.NewJSONHandler(logSink, nil))
	supervisor := guest.NewSupervisor(logger)

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	if err := serveGuest(ctx, supervisor, uint32(*port), uint32(*landPort), uint32(*notifyPort), logger); err != nil {
		logger.Error("mgit-guest exited with error", "error", err.Error())
		return 1
	}
	return 0
}
