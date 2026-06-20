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
}

// NewManager returns a microVM manager backed by Virtualization.framework.
func NewManager(cfg Config) (*microvm.Manager, error) {
	hv := cfg.Hypervisor
	if hv == nil {
		var err error
		hv, err = newPlatformHypervisor()
		if err != nil {
			return nil, err
		}
	}
	return microvm.NewManager(microvm.Config{
		Backend:    model.BackendVZF,
		WorkDir:    cfg.WorkDir,
		Resolve:    cfg.Resolve,
		Hypervisor: hv,
		PeerBinder: cfg.PeerBinder,
		Logger:     cfg.Logger,
		Clock:      cfg.Clock,
	})
}
