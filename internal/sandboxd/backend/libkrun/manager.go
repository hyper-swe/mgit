package libkrun

import (
	"log/slog"
	"time"

	"github.com/hyper-swe/mgit/internal/model"
	"github.com/hyper-swe/mgit/internal/sandboxd/backend/microvm"
	"github.com/hyper-swe/mgit/internal/sandboxd/provision"
)

// Config wires the libkrun manager. Like the firecracker and vzf managers it
// is a thin platform seam: the lifecycle lives in the shared microvm package;
// this package supplies the re-exec Hypervisor and the per-port vsock
// dialers.
type Config struct {
	WorkDir string
	Resolve func(imageRef string) (microvm.ImagePaths, error)
	Logger  *slog.Logger
	Clock   func() time.Time
	// PeerBinder records each sandbox's host-observed peer identity for
	// channel authorization (SEC-10); nil disables binding.
	PeerBinder microvm.PeerBinder
	// NotifyRegistrar starts/stops each sandbox's per-VM guest->host notify
	// listener (the auto-land trigger); nil disables auto-land. Refs: MGIT-11.10.11
	NotifyRegistrar microvm.NotifyRegistrar
	// StoreProvisioner seeds the SEC-03 private store per launch. NOTE: the
	// libkrun backend cannot DELIVER the quarantined layout yet — CreateVM
	// refuses a launch carrying a private store rather than sharing the live
	// worktree unquarantined (fail closed). Wiring the provisioner is still
	// correct: the refusal, not a silent downgrade, is the contract. Refs: SEC-03
	StoreProvisioner provision.Provisioner
	// SensitivePaths are the worktree-relative host-trusted patterns for the
	// guest plan (FR-17.14); only used once staged delivery lands.
	SensitivePaths []string
}

// NewManager returns a microVM manager backed by libkrun re-exec children.
func NewManager(cfg Config) (*microvm.Manager, error) {
	hv, err := NewHypervisor(cfg.Logger)
	if err != nil {
		return nil, err
	}
	return microvm.NewManager(microvm.Config{
		Backend:          model.BackendLibkrun,
		WorkDir:          cfg.WorkDir,
		Resolve:          cfg.Resolve,
		Hypervisor:       hv,
		GuestDialer:      newGuestDialer(cfg.WorkDir),
		PeerBinder:       cfg.PeerBinder,
		NotifyRegistrar:  cfg.NotifyRegistrar,
		StoreProvisioner: cfg.StoreProvisioner,
		SensitivePaths:   cfg.SensitivePaths,
		Logger:           cfg.Logger,
		Clock:            cfg.Clock,
	})
}
