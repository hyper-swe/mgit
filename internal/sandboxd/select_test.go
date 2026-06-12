// Package sandboxd tests verify backend selection per MGIT-11.5.4
// acceptance criteria: the reduced-isolation container fallback is
// never selected automatically and requires the explicit, audited
// acknowledgment. Refs: FR-17.15
package sandboxd

import (
	"context"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/hyper-swe/mgit/internal/model"
)

func selectionFixtures() (BackendFactories, *recordingAudit) {
	audit := &recordingAudit{}
	factories := BackendFactories{
		Hypervisor: func() (model.SandboxManager, error) {
			return newFakeManager(), nil
		},
		Container: func() (model.SandboxManager, error) {
			return newFakeManager("01JXCONTAINER"), nil
		},
	}
	return factories, audit
}

// recordingAudit captures backend-selection audit events.
type recordingAudit struct {
	details []string
	fail    bool
}

func (r *recordingAudit) RecordBackendSelection(_ context.Context, detail string) error {
	if r.fail {
		return assert.AnError
	}
	r.details = append(r.details, detail)
	return nil
}

// TestFallback_RequiresExplicitAck verifies the double opt-in: backend
// container plus the acknowledgment selects the fallback (and nothing
// less does). Refs: FR-17.15
func TestFallback_RequiresExplicitAck(t *testing.T) {
	factories, audit := selectionFixtures()
	ctx := context.Background()

	mgr, err := SelectBackend(ctx, SelectOptions{
		Backend:                     BackendRequestContainer,
		AcknowledgeReducedIsolation: true,
		Audit:                       audit,
	}, factories)
	require.NoError(t, err)
	require.NotNil(t, mgr)

	listed, err := mgr.List(ctx)
	require.NoError(t, err)
	require.Len(t, listed, 1, "the container factory's manager is returned")
	assert.Equal(t, "01JXCONTAINER", listed[0].ID)
}

// TestFallback_NoAck_BackendUnavailable verifies fail-closed selection:
// no acknowledgment means no fallback, and a missing hypervisor NEVER
// silently downgrades to the container. Refs: FR-17.15
func TestFallback_NoAck_BackendUnavailable(t *testing.T) {
	ctx := context.Background()

	t.Run("container_without_ack_refused", func(t *testing.T) {
		factories, audit := selectionFixtures()
		_, err := SelectBackend(ctx, SelectOptions{
			Backend: BackendRequestContainer,
			Audit:   audit,
		}, factories)
		assert.ErrorIs(t, err, model.ErrSandboxBackendUnavailable)
		assert.Empty(t, audit.details, "a refused selection records nothing")
	})

	t.Run("missing_hypervisor_never_downgrades", func(t *testing.T) {
		_, audit := selectionFixtures()
		factories := BackendFactories{
			Hypervisor: func() (model.SandboxManager, error) {
				return nil, model.ErrSandboxBackendUnavailable
			},
			Container: func() (model.SandboxManager, error) {
				t.Fatal("the container factory must never be consulted for an auto selection")
				return nil, nil
			},
		}
		_, err := SelectBackend(ctx, SelectOptions{Backend: BackendRequestAuto, Audit: audit}, factories)
		assert.ErrorIs(t, err, model.ErrSandboxBackendUnavailable)
	})

	t.Run("ack_without_container_backend_is_not_a_downgrade_license", func(t *testing.T) {
		factories, audit := selectionFixtures()
		factories.Hypervisor = func() (model.SandboxManager, error) {
			return nil, model.ErrSandboxBackendUnavailable
		}
		_, err := SelectBackend(ctx, SelectOptions{
			Backend:                     BackendRequestAuto,
			AcknowledgeReducedIsolation: true,
			Audit:                       audit,
		}, factories)
		assert.ErrorIs(t, err, model.ErrSandboxBackendUnavailable,
			"the ack flag alone must not switch backends")
	})

	t.Run("unknown_backend_rejected", func(t *testing.T) {
		factories, audit := selectionFixtures()
		_, err := SelectBackend(ctx, SelectOptions{Backend: "chroot", Audit: audit}, factories)
		assert.Error(t, err)
	})
}

// TestFallback_AckRecordedInAudit verifies the acknowledgment is an
// audit event; an unrecordable acknowledgment is never applied.
// Refs: FR-17.15, FR-17.18
func TestFallback_AckRecordedInAudit(t *testing.T) {
	ctx := context.Background()

	factories, audit := selectionFixtures()
	_, err := SelectBackend(ctx, SelectOptions{
		Backend:                     BackendRequestContainer,
		AcknowledgeReducedIsolation: true,
		Audit:                       audit,
	}, factories)
	require.NoError(t, err)

	require.Len(t, audit.details, 1)
	assert.True(t, strings.Contains(audit.details[0], "reduced_isolation"),
		"the audit record names the reduced assurance, got %q", audit.details[0])
	assert.Contains(t, audit.details[0], model.BackendContainer)

	t.Run("unrecordable_ack_not_applied", func(t *testing.T) {
		factories, audit := selectionFixtures()
		audit.fail = true
		_, err := SelectBackend(ctx, SelectOptions{
			Backend:                     BackendRequestContainer,
			AcknowledgeReducedIsolation: true,
			Audit:                       audit,
		}, factories)
		assert.Error(t, err, "selection without its audit record must fail")
	})

	t.Run("nil_audit_rejected_for_container", func(t *testing.T) {
		factories, _ := selectionFixtures()
		_, err := SelectBackend(ctx, SelectOptions{
			Backend:                     BackendRequestContainer,
			AcknowledgeReducedIsolation: true,
		}, factories)
		assert.Error(t, err, "an unauditable acknowledgment is no acknowledgment")
	})
}
