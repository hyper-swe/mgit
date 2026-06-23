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
	"syscall"
	"time"

	"github.com/hyper-swe/mgit/internal/sandboxd"
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

// daemonOpts is the parsed command-line configuration.
type daemonOpts struct {
	socket       string
	hostRoot     string
	repoRoot     string
	workDir      string
	backend      string
	idleGrace    time.Duration
	maxSandboxes int
	maxMemoryMB  int
	maxConns     int
	ackReduced   bool
}

// parseFlags parses argv. It returns nil opts with an exit code when the
// caller should stop (help → 0, parse error → 2).
func parseFlags(args []string, logSink io.Writer) (*daemonOpts, int) {
	flags := flag.NewFlagSet("mgit-sandboxd", flag.ContinueOnError)
	flags.SetOutput(logSink)
	o := &daemonOpts{}
	flags.StringVar(&o.socket, "socket", "", "unix socket path to serve (required)")
	flags.StringVar(&o.hostRoot, "host-root", "", "host config root holding images.lock + trust root (FR-17.13)")
	flags.StringVar(&o.repoRoot, "repo-root", "", "mgit repository root the land path imports into (defaults to the host-root's repo)")
	flags.StringVar(&o.workDir, "work-dir", "", "sandbox-local state root (overlays, sockets); never a worktree")
	flags.DurationVar(&o.idleGrace, "idle-grace", 30*time.Second, "zero-sandbox linger before exit")
	flags.IntVar(&o.maxSandboxes, "max-sandboxes", 8, "global concurrent-sandbox ceiling (FR-17.26)")
	flags.IntVar(&o.maxMemoryMB, "max-memory-mb", 0, "global sandbox memory ceiling in MB (0 until policy wiring resolves the FR-17.26 50% host default)")
	flags.IntVar(&o.maxConns, "max-conns", 0, "max concurrent control connections (0 = daemon default)")
	flags.StringVar(&o.backend, "backend", sandboxd.BackendRequestAuto,
		"sandbox backend: auto (platform hypervisor) or container (REDUCED isolation; requires --acknowledge-reduced-isolation)")
	flags.BoolVar(&o.ackReduced, "acknowledge-reduced-isolation", false,
		"accept the container fallback's shared-kernel risk (recorded in the audit trail)")
	if err := flags.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return nil, 0
		}
		return nil, 2
	}
	return o, 0
}

// run wires flags into the daemon and blocks until exit. Split from
// main for testability (DI; no globals).
func run(args []string, logSink io.Writer) int {
	opts, code := parseFlags(args, logSink)
	if opts == nil {
		return code
	}
	logger := slog.New(slog.NewJSONHandler(logSink, nil))
	if opts.socket == "" {
		logger.Error("missing required flag", "flag", "socket")
		return 2
	}
	clock := func() time.Time { return time.Now().UTC() }

	// One PeerBinder is shared: the backend Binds each launch / Invalidates
	// each teardown to its host-observed peer identity, and the daemon owns
	// it to authorize incoming guest->host channels against those bindings
	// (SEC-10, the land/attestation accept path). Refs: FR-17.27
	peerBinder := sandboxd.NewPeerBinder(logger)

	selected, err := selectManager(backendSelection{
		backend: opts.backend, ackReduced: opts.ackReduced,
		hostRoot: opts.hostRoot, repoRoot: opts.repoRoot, workDir: opts.workDir,
		logger: logger, clock: clock,
		peerBinder: peerBinder,
	})
	if err != nil {
		logger.Error("sandbox backend selection failed", "error", err.Error())
		return 2
	}

	// The ceiling wraps whichever backend was selected: launches never
	// reach a backend unadmitted (SEC-09).
	manager := sandboxd.NewCeilingManager(selected, opts.maxSandboxes, opts.maxMemoryMB, 0)

	dcfg := sandboxd.Config{
		SocketPath: opts.socket, Manager: manager,
		Logger: logger, Clock: clock, IdleGrace: opts.idleGrace, MaxConns: opts.maxConns,
		PeerBinder: peerBinder,
	}
	// Wire the dispatch service when a host root is configured: the daemon
	// then serves launch/exec/list/remove/status (going through the
	// service, never the manager). Without it the daemon greets only — a
	// loud warning, never a silent half-serving daemon.
	if opts.hostRoot != "" {
		svc, events, policyStore, closeAudit, svcErr := buildSandboxService(manager, opts.hostRoot, clock, logger)
		if svcErr != nil {
			logger.Error("sandbox service wiring failed", "error", svcErr.Error())
			return 2
		}
		defer func() { _ = closeAudit() }()
		dcfg.Service = svc

		// Wire host egress enforcement (allowlist proxy + restricted DNS) so
		// the service starts/stops it across each allowlist sandbox's
		// lifecycle, and capability escalation (deny->prompt->grant). No-op off
		// Linux and for none/open sandboxes. Refs: FR-17.8, FR-17.12
		if capSvc := wireEgress(svc, events, clock, logger); capSvc != nil {
			dcfg.Grants = capSvc
		}

		// Wire the land path when the host repo is reachable. A failure here is
		// non-fatal: the daemon still serves launch/exec/list/remove/status,
		// but `mgit sandbox land` reports "not served" until land is wired.
		lander, closeLand, landErr := buildLandService(opts.hostRoot, opts.repoRoot, opts.workDir, svc, events, policyStore, peerBinder, clock, logger)
		if landErr != nil {
			logger.Warn("sandbox land path not wired; land will not be served",
				"event", "land_unwired", "error", landErr.Error())
		} else {
			defer func() { _ = closeLand() }()
			dcfg.Lander = lander
		}
	} else {
		logger.Warn("sandboxd serving greet-only: --host-root not set; no sandbox operations will be served",
			"event", "greet_only")
	}

	daemon, err := sandboxd.New(dcfg)
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
