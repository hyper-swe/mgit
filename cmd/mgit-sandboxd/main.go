// Command mgit-sandboxd is the per-platform sandbox helper daemon
// (FR-17.16): it owns VMM control (and any CGO) so core mgit stays
// pure-Go, serves an authenticated local unix socket, and exits when
// no sandboxes remain. Backends are wired in per platform
// (MGIT-11.5.x); a build without one refuses launches with
// ErrSandboxBackendUnavailable. Every manager is wrapped in the
// FR-17.26 global ceiling — there is no un-ceilinged launch path.
// Refs: FR-17.16, FR-17.26, NFR-17.6, MGIT-11.4.1
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/signal"
	"runtime"
	"syscall"
	"time"

	"github.com/hyper-swe/mgit/internal/model"
	"github.com/hyper-swe/mgit/internal/sandboxd"
	"github.com/hyper-swe/mgit/internal/sandboxd/backend/container"
)

// slogBackendAuditor records backend selections in the daemon's
// structured log; the durable sandbox_events record rides the service
// wiring (MGIT-11.9.x), which also audits each launch.
type slogBackendAuditor struct {
	logger *slog.Logger
}

// RecordBackendSelection logs one selection event.
func (a slogBackendAuditor) RecordBackendSelection(_ context.Context, detail string) error {
	a.logger.Warn("sandbox backend selected with reduced isolation",
		"event", "backend_selected", "detail", detail)
	return nil
}

func main() {
	os.Exit(run(os.Args[1:], os.Stderr))
}

// run wires flags into the daemon and blocks until exit. Split from
// main for testability (DI; no globals).
func run(args []string, logSink io.Writer) int {
	flags := flag.NewFlagSet("mgit-sandboxd", flag.ContinueOnError)
	flags.SetOutput(logSink)
	socket := flags.String("socket", "", "unix socket path to serve (required)")
	idleGrace := flags.Duration("idle-grace", 30*time.Second, "zero-sandbox linger before exit")
	maxSandboxes := flags.Int("max-sandboxes", 8, "global concurrent-sandbox ceiling (FR-17.26)")
	maxMemoryMB := flags.Int("max-memory-mb", 0, "global sandbox memory ceiling in MB (0 until policy wiring resolves the FR-17.26 50% host default)")
	backend := flags.String("backend", sandboxd.BackendRequestAuto,
		"sandbox backend: auto (platform hypervisor) or container (REDUCED isolation; requires --acknowledge-reduced-isolation)")
	ackReduced := flags.Bool("acknowledge-reduced-isolation", false,
		"accept the container fallback's shared-kernel risk (recorded in the audit trail)")
	if err := flags.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return 2
	}

	logger := slog.New(slog.NewJSONHandler(logSink, nil))
	if *socket == "" {
		logger.Error("missing required flag", "flag", "socket")
		return 2
	}

	selected, err := sandboxd.SelectBackend(context.Background(), sandboxd.SelectOptions{
		Backend:                     *backend,
		AcknowledgeReducedIsolation: *ackReduced,
		Audit:                       slogBackendAuditor{logger: logger},
	}, sandboxd.BackendFactories{
		// The hypervisor factory wires the platform microVM backend once
		// image resolution exists (images.lock, MGIT-11.5.5); until then
		// launches refuse honestly.
		Hypervisor: func() (model.SandboxManager, error) {
			return sandboxd.NewUnavailableManager(runtime.GOOS), nil
		},
		Container: func() (model.SandboxManager, error) {
			return container.NewManager(container.Config{
				Runner:         container.PodmanRunner{},
				SensitivePaths: model.DefaultSandboxPolicy().SensitivePaths,
				Logger:         logger,
				Clock:          func() time.Time { return time.Now().UTC() },
			})
		},
	})
	if err != nil {
		logger.Error("sandbox backend selection failed", "error", err.Error())
		return 2
	}

	// The ceiling wraps whichever backend was selected: launches never
	// reach a backend unadmitted (SEC-09).
	manager := sandboxd.NewCeilingManager(selected, *maxSandboxes, *maxMemoryMB, 0)

	daemon, err := sandboxd.New(sandboxd.Config{
		SocketPath: *socket,
		Manager:    manager,
		Logger:     logger,
		Clock:      func() time.Time { return time.Now().UTC() },
		IdleGrace:  *idleGrace,
	})
	if err != nil {
		logger.Error("sandboxd configuration invalid", "error", err)
		return 2
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	if err := daemon.Run(ctx); err != nil {
		logger.Error("sandboxd exited with error", "error", fmt.Sprintf("%v", err))
		return 1
	}
	return 0
}
