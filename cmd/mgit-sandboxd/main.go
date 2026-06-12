// Command mgit-sandboxd is the per-platform sandbox helper daemon
// (FR-17.16): it owns VMM control (and any CGO) so core mgit stays
// pure-Go, serves a local unix socket, and exits when no sandboxes
// remain. Backends are wired in per platform (MGIT-11.5.x); a build
// without one refuses launches with ErrSandboxBackendUnavailable.
// Refs: FR-17.16, NFR-17.6, MGIT-11.4.1
package main

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"runtime"
	"syscall"
	"time"

	"github.com/hyper-swe/mgit/internal/sandboxd"
)

func main() {
	os.Exit(run(os.Args[1:], os.Stderr))
}

// run wires flags into the daemon and blocks until exit. Split from
// main for testability (DI; no globals).
func run(args []string, logSink *os.File) int {
	flags := flag.NewFlagSet("mgit-sandboxd", flag.ContinueOnError)
	socket := flags.String("socket", "", "unix socket path to serve (required)")
	idleGrace := flags.Duration("idle-grace", 30*time.Second, "zero-sandbox linger before exit")
	if err := flags.Parse(args); err != nil {
		return 2
	}

	logger := slog.New(slog.NewJSONHandler(logSink, nil))
	if *socket == "" {
		logger.Error("missing required flag", "flag", "socket")
		return 2
	}

	daemon, err := sandboxd.New(sandboxd.Config{
		SocketPath: *socket,
		Manager:    sandboxd.NewUnavailableManager(runtime.GOOS),
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
