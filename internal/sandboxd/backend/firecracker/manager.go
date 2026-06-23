// Package firecracker is the Linux KVM sandbox backend (FR-17.15). Like
// vzf it is a thin platform seam: the lifecycle lives in the shared
// microvm package, and this package supplies a Firecracker-class
// Hypervisor implementation. The Firecracker SDK is pure Go (it drives
// the VMM over a unix-socket HTTP API), so unlike vzf this backend needs
// no CGO; core mgit stays pure-Go regardless. Refs: FR-17.15, FR-17.16
package firecracker

import (
	"log/slog"
	"time"

	"github.com/hyper-swe/mgit/internal/model"
	"github.com/hyper-swe/mgit/internal/sandboxd/backend/microvm"
	"github.com/hyper-swe/mgit/internal/sandboxd/provision"
)

// ImagePaths re-exports the shared type so callers keep a stable API.
type ImagePaths = microvm.ImagePaths

// Config wires the KVM manager. A nil Hypervisor selects the platform
// (Firecracker-on-KVM) implementation; on non-linux builds that is
// ErrSandboxBackendUnavailable.
type Config struct {
	WorkDir string
	Resolve func(imageRef string) (ImagePaths, error)
	// Hypervisor is the platform host; nil selects the Firecracker
	// implementation. Injectable for tests.
	Hypervisor microvm.Hypervisor
	Logger     *slog.Logger
	Clock      func() time.Time
	// FirecrackerBin is the path to the firecracker binary. Empty
	// resolves "firecracker" from PATH. Ignored when Hypervisor is set.
	FirecrackerBin string
	// ExtIface is the host external interface open mode NATs the guest out
	// through. Empty auto-detects the default-route interface; the
	// default-safe modes (none/allowlist) never NAT and ignore it. Ignored
	// when Hypervisor is set. Refs: FR-17.7
	ExtIface string
	// PeerBinder records each sandbox's host-observed peer identity for
	// channel authorization (SEC-10); nil disables binding.
	PeerBinder microvm.PeerBinder
	// StoreProvisioner seeds the SEC-03 private, sandbox-local store per launch
	// and supplies the shared store path for the non-reachability check. When
	// set, the quarantine control is realized; nil leaves the pre-SEC-03 direct
	// delivery (tests/direct path). Refs: SEC-03
	StoreProvisioner provision.Provisioner
	// SensitivePaths are the worktree-relative host-trusted patterns layered
	// read-only into the guest plan (FR-17.14). Refs: FR-17.14
	SensitivePaths []string
}

// NewManager returns a microVM manager backed by Firecracker on KVM.
func NewManager(cfg Config) (*microvm.Manager, error) {
	hv := cfg.Hypervisor
	if hv == nil {
		var err error
		hv, err = newPlatformHypervisor(cfg.FirecrackerBin, cfg.ExtIface)
		if err != nil {
			return nil, err
		}
	}
	return microvm.NewManager(microvm.Config{
		Backend:          model.BackendKVM,
		WorkDir:          cfg.WorkDir,
		Resolve:          cfg.Resolve,
		Hypervisor:       hv,
		GuestDialer:      newGuestDialer(cfg.WorkDir),
		PeerBinder:       cfg.PeerBinder,
		StoreProvisioner: cfg.StoreProvisioner,
		SensitivePaths:   cfg.SensitivePaths,
		Logger:           cfg.Logger,
		Clock:            cfg.Clock,
	})
}
