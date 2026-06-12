package sandboxd

import (
	"context"
	"fmt"

	"github.com/hyper-swe/mgit/internal/model"
)

// UnavailableManager is the SandboxManager for platforms (or builds)
// with no hypervisor backend compiled in: Launch reports
// ErrSandboxBackendUnavailable per FR-17.15 and nothing is ever
// supervised. This is the specified no-backend behavior, not a stub —
// platform backends (MGIT-11.5.x) replace it via build wiring.
// Refs: FR-17.15, FR-17.20
type UnavailableManager struct {
	platform string
}

// NewUnavailableManager names the platform for error context.
func NewUnavailableManager(platform string) *UnavailableManager {
	return &UnavailableManager{platform: platform}
}

// Launch always refuses: no backend exists on this build.
func (u *UnavailableManager) Launch(_ context.Context, _ model.SandboxLaunchOptions) (*model.SandboxInfo, error) {
	return nil, fmt.Errorf("%w: %s", model.ErrSandboxBackendUnavailable, u.platform)
}

// List reports no sandboxes (none can exist without a backend).
func (u *UnavailableManager) List(_ context.Context) ([]model.SandboxInfo, error) {
	return nil, nil
}

// Exec refuses: nothing can be running.
func (u *UnavailableManager) Exec(_ context.Context, id string, _ model.ExecRequest) (*model.ExecResult, error) {
	return nil, fmt.Errorf("%w: %q", model.ErrSandboxNotFound, id)
}

// Stop refuses: nothing can be running.
func (u *UnavailableManager) Stop(_ context.Context, id string, _ bool) error {
	return fmt.Errorf("%w: %q", model.ErrSandboxNotFound, id)
}

// Remove refuses: nothing can be running.
func (u *UnavailableManager) Remove(_ context.Context, id string, _ bool) error {
	return fmt.Errorf("%w: %q", model.ErrSandboxNotFound, id)
}

// Resolve refuses: nothing can be running.
func (u *UnavailableManager) Resolve(_ context.Context, id string) (*model.SandboxInfo, error) {
	return nil, fmt.Errorf("%w: %q", model.ErrSandboxNotFound, id)
}
