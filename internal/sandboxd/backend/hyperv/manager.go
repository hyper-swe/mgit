// Package hyperv is a fail-closed placeholder for the Windows sandbox
// backend (FR-17.15). No Windows backend ships in v1: the microVM
// sandbox is Linux + macOS only, and Windows runs core mgit without the
// sandbox (ADR-006, FR-17.39). newPlatformHost therefore returns
// ErrSandboxBackendUnavailable; the manager wiring below is a skeleton
// kept for interface parity, not a working backend.
//
// The real Windows backend is deferred to epic MGIT-12 and will run a
// host-matching WINDOWS guest via a Hyper-V-isolated Windows container
// (WCOW), driven through the HCS API / containerd — NOT a Linux guest
// (LCOW), and NOT the kernel+rootfs microvm.Manager seam this skeleton
// happens to wire. Prerequisites are documented in Prerequisites().
// Refs: FR-17.15, FR-17.39, ADR-006
package hyperv

import (
	"log/slog"
	"time"

	"github.com/hyper-swe/mgit/internal/model"
	"github.com/hyper-swe/mgit/internal/sandboxd/backend/microvm"
)

// ImagePaths re-exports the shared type so callers keep a stable API.
type ImagePaths = microvm.ImagePaths

// Prerequisites documents what a Windows host needs to run this
// backend. Surfaced so operators (and TestHyperV_PrereqsDocumented)
// have a single authoritative list. Refs: FR-17.15
const Prerequisites = `mgit Hyper-V sandbox backend prerequisites (Windows):
  - Windows 10/11 Pro or Enterprise, or Windows Server, 64-bit.
  - The Hyper-V platform feature enabled:
      Enable-WindowsOptionalFeature -Online -FeatureName Microsoft-Hyper-V -All
    (a reboot is required after enabling).
  - The Windows Hypervisor Platform (WHP) available; hardware
    virtualization (VT-x/AMD-V) and SLAT enabled in firmware.
  - mgit-sandboxd must run with Administrator privileges: the Host
    Compute Service (HCS) requires elevation to create utility VMs.
  - Nested virtualization required only when the host is itself a VM.`

// Config wires the Hyper-V manager. Field naming mirrors vzf.Config
// for cross-backend consistency (FR-17.15).
type Config struct {
	WorkDir string
	Resolve func(imageRef string) (ImagePaths, error)
	// Hypervisor is the platform host; nil selects the HCS
	// implementation (newPlatformHost). Injectable for tests.
	Hypervisor microvm.Hypervisor
	Logger     *slog.Logger
	Clock      func() time.Time
}

// NewManager returns a microVM manager backed by Hyper-V/HCS.
func NewManager(cfg Config) (*microvm.Manager, error) {
	host := cfg.Hypervisor
	if host == nil {
		var err error
		host, err = newPlatformHost()
		if err != nil {
			return nil, err
		}
	}
	return microvm.NewManager(microvm.Config{
		Backend:    model.BackendHyperV,
		WorkDir:    cfg.WorkDir,
		Resolve:    cfg.Resolve,
		Hypervisor: host,
		Logger:     cfg.Logger,
		Clock:      cfg.Clock,
	})
}
