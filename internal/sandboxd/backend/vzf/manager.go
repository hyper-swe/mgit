// Package vzf is the macOS Virtualization.framework sandbox backend
// (FR-17.15). It is a thin platform seam: the lifecycle lives in the
// shared microvm package, and this package supplies the vz Hypervisor
// implementation (CGO confined to the darwin-tagged file, core mgit
// stays pure-Go). Refs: FR-17.15, FR-17.16
package vzf

import (
	"log/slog"
	"time"

	"github.com/hyper-swe/mgit/internal/model"
	"github.com/hyper-swe/mgit/internal/sandboxd/backend/microvm"
	"github.com/hyper-swe/mgit/internal/sandboxd/provision"
)

// ImagePaths re-exports the shared type so callers keep a stable API.
type ImagePaths = microvm.ImagePaths

// Config wires the vzf manager. A nil Hypervisor selects the platform
// (Virtualization.framework) implementation; on non-darwin / CGO-free
// builds that is ErrSandboxBackendUnavailable.
type Config struct {
	WorkDir string
	Resolve func(imageRef string) (ImagePaths, error)
	// Hypervisor is the platform host; nil selects the
	// Virtualization.framework implementation. Injectable for tests.
	Hypervisor microvm.Hypervisor
	Logger     *slog.Logger
	Clock      func() time.Time
	// PeerBinder records each sandbox's host-observed peer identity for
	// channel authorization (SEC-10); nil disables binding.
	PeerBinder microvm.PeerBinder
	// StoreProvisioner seeds the SEC-03 private, sandbox-local store per launch
	// and supplies the shared store path for the non-reachability check. When
	// set, the quarantine control is realized: the shared microvm manager builds
	// the plan + binds the private store + fails closed (ErrSharedStoreReachable),
	// and the vz hypervisor shares a STAGED worktree (worktree files + the
	// private .mgit, in-worktree stores excluded, escaping symlinks rejected)
	// rather than the live worktree. nil leaves the pre-SEC-03 live share
	// (tests/direct path). Refs: SEC-03
	StoreProvisioner provision.Provisioner
	// SensitivePaths are the worktree-relative host-trusted patterns layered
	// read-only into the guest plan (FR-17.14). Refs: FR-17.14
	SensitivePaths []string
}

// NewManager returns a microVM manager backed by Virtualization.framework.
// The live-VM registry is shared between the platform hypervisor (which
// publishes each started VM into it) and the guest dialer (which resolves a
// sandbox ID to its live handle to connect), so exec/land reach the running
// guest's vsock over the framework API. Refs: FR-17.15, FR-17.16
func NewManager(cfg Config) (*microvm.Manager, error) {
	mgr, _, err := NewManagerWithLand(cfg)
	return mgr, err
}

// NewManagerWithLand returns the vzf microVM manager AND its host LAND
// dialer, both bound to ONE live-VM registry so they resolve a sandbox to
// the same running VZVirtualMachine: the platform hypervisor publishes each
// started VM into the registry, the manager's exec dialer reaches it on the
// exec port, and the returned land dialer reaches it on the LAND port over
// the same VZVirtioSocketDevice.Connect. The daemon land wiring uses this to
// select the vzf land transport on macOS (firecracker on Linux) behind the
// microvm.GuestDialer seam. The land dialer fails closed when no live VM is
// registered for the sandbox. Refs: FR-17.5, FR-17.15, FR-17.16
func NewManagerWithLand(cfg Config) (*microvm.Manager, microvm.GuestDialer, error) {
	reg := newLiveVMs()
	hv := cfg.Hypervisor
	if hv == nil {
		var err error
		hv, err = newPlatformHypervisor(reg)
		if err != nil {
			return nil, nil, err
		}
	}
	mgr, err := microvm.NewManager(microvm.Config{
		Backend:          model.BackendVZF,
		WorkDir:          cfg.WorkDir,
		Resolve:          cfg.Resolve,
		Hypervisor:       hv,
		GuestDialer:      newGuestExecDialer(reg),
		PeerBinder:       cfg.PeerBinder,
		StoreProvisioner: cfg.StoreProvisioner,
		SensitivePaths:   cfg.SensitivePaths,
		Logger:           cfg.Logger,
		Clock:            cfg.Clock,
	})
	if err != nil {
		return nil, nil, err
	}
	return mgr, newGuestLandDialer(reg), nil
}
