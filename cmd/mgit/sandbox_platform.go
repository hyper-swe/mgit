package main

import (
	"fmt"
	"runtime"

	"github.com/hyper-swe/mgit/internal/model"
)

// sandboxHostOS is the host platform the sandbox commands check. It is a
// variable so a test can name another platform.
var sandboxHostOS = runtime.GOOS

// sandboxPlatformRefusal returns the reason the sandbox cannot run on goos, or
// nil when a backend exists for it. Windows has no backend yet, so every
// command that needs the sandbox stops here with the reason, before it looks
// for a repository or a daemon. It never falls back to running the work on the
// host. The error wraps ErrSandboxBackendUnavailable so callers classify it
// like any other missing backend. Refs: FR-17.15, ADR-006
func sandboxPlatformRefusal(goos string) error {
	if goos != "windows" {
		return nil
	}
	return fmt.Errorf("%w: the sandbox is not available on Windows yet. "+
		"mgit has no sandbox backend for Windows, so it cannot confine task work there. "+
		"Version control commands work as usual. Run sandboxed work on Linux or macOS (Apple silicon). "+
		"Do not run untrusted code directly on this host instead. "+
		"The plan for Windows is in docs/adr/017-windows-wsl2-sandbox.md",
		model.ErrSandboxBackendUnavailable)
}
