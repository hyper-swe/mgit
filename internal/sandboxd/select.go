package sandboxd

import (
	"context"
	"fmt"

	"github.com/hyper-swe/mgit/internal/model"
)

// Backend selection requests. The container fallback is NEVER part of
// auto selection: a missing hypervisor is a hard failure, not a silent
// downgrade (FR-17.15, ADR-005). Refs: FR-17.15
const (
	// BackendRequestAuto selects the platform hypervisor backend.
	BackendRequestAuto = "auto"
	// BackendRequestContainer selects the reduced-isolation fallback;
	// valid only with the explicit acknowledgment.
	BackendRequestContainer = "container"
)

// BackendAuditor records backend-selection events; the reduced-
// isolation acknowledgment MUST be on the audit trail (FR-17.15).
type BackendAuditor interface {
	RecordBackendSelection(ctx context.Context, detail string) error
}

// SelectOptions is one backend-selection request.
type SelectOptions struct {
	Backend                     string         // BackendRequestAuto (default) | BackendRequestContainer
	AcknowledgeReducedIsolation bool           // --acknowledge-reduced-isolation
	Audit                       BackendAuditor // required when the container is requested
}

// BackendFactories supplies the constructible backends.
type BackendFactories struct {
	Hypervisor func() (model.SandboxManager, error) // platform microVM backend
	Container  func() (model.SandboxManager, error) // reduced-isolation fallback
}

// SelectBackend resolves a manager from the request. Selection is
// fail-closed: the container fallback requires BOTH the explicit
// backend request and the acknowledgment, the acknowledgment is
// recorded in the audit trail before the backend is handed out (an
// unrecordable acknowledgment is never applied), and no code path
// downgrades a missing hypervisor to the container. Refs: FR-17.15
func SelectBackend(ctx context.Context, opts SelectOptions, factories BackendFactories) (model.SandboxManager, error) {
	switch opts.Backend {
	case "", BackendRequestAuto:
		return factories.Hypervisor()

	case BackendRequestContainer:
		if !opts.AcknowledgeReducedIsolation {
			return nil, fmt.Errorf(
				"%w: the container backend trades the hardware boundary for a shared kernel; pass --acknowledge-reduced-isolation to accept that",
				model.ErrSandboxBackendUnavailable)
		}
		if opts.Audit == nil {
			return nil, fmt.Errorf(
				"%w: the reduced-isolation acknowledgment requires an audit recorder",
				model.ErrSandboxBackendUnavailable)
		}
		detail := fmt.Sprintf(`{"backend":%q,"reduced_isolation":true,"acknowledged":true}`,
			model.BackendContainer)
		if err := opts.Audit.RecordBackendSelection(ctx, detail); err != nil {
			return nil, fmt.Errorf("record reduced-isolation acknowledgment (selection not applied): %w", err)
		}
		return factories.Container()

	default:
		return nil, fmt.Errorf("%w: unknown backend %q", model.ErrSandboxBackendUnavailable, opts.Backend)
	}
}
