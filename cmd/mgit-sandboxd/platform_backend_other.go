//go:build !linux && !darwin

package main

import (
	"runtime"

	"github.com/hyper-swe/mgit/internal/model"
	"github.com/hyper-swe/mgit/internal/sandboxd"
	"github.com/hyper-swe/mgit/internal/sandboxd/backend/microvm"
)

// newHypervisorBackend reports that the microVM sandbox is unavailable
// on this platform. v1 ships the sandbox on Linux (KVM) and macOS
// (Virtualization.framework) only; Windows — and any other OS — runs
// core mgit WITHOUT sandboxing until the host-matching WCOW backend
// lands (ADR-006, FR-17.39, epic MGIT-12).
//
// This returns a working manager (not an error) so the daemon still
// starts on these hosts and individual launches fail gracefully with a
// clear "backend unavailable" message — never a crash, never a silent
// downgrade to the container fallback (FR-17.15). The seam is identical
// to the Linux/macOS factories, so the Windows backend slots in as a
// build-tagged sibling with no change to this caller or main.
//
// The land dialer is nil here: there is no guest transport on a platform
// without a sandbox backend, so the daemon land wiring reports land "not
// served" (it never half-wires a land path with no transport). Refs: MGIT-13.1.1
func newHypervisorBackend(_ hypervisorDeps) (model.SandboxManager, microvm.GuestDialer, error) {
	return sandboxd.NewUnavailableManager(runtime.GOOS), nil, nil
}
