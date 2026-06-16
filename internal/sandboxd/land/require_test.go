package land

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/hyper-swe/mgit/internal/model"
	"github.com/hyper-swe/mgit/internal/sandboxd/attest"
)

// realAttestor builds a host attest.Service and a valid attestation for
// the integration-style cases (the gate composed with the real verifier).
func realAttestor(t *testing.T) (*attest.Service, *model.Attestation) {
	t.Helper()
	hostRoot := t.TempDir()
	require.NoError(t, attest.GenerateKey(context.Background(), hostRoot, noopKeyAuditor{}))
	svc, err := attest.NewService(hostRoot, func() time.Time { return time.Unix(0, 0).UTC() })
	require.NoError(t, err)
	att, err := svc.Attest(context.Background(), "01JXSBLAND00000000000000000",
		"0123456789abcdef0123456789abcdef01234567",
		"0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef")
	require.NoError(t, err)
	return svc, att
}

type noopKeyAuditor struct{}

func (noopKeyAuditor) RecordKeyChange(context.Context, string) error { return nil }

// TestRequireSandbox_UnattestedCommit_Rejected verifies that with the
// policy on, a commit without an attestation is refused. Refs: FR-17.6, F-02
func TestRequireSandbox_UnattestedCommit_Rejected(t *testing.T) {
	verifyCalled := false
	sid, err := EnforceRequireSandbox(context.Background(), true, nil,
		func(context.Context, *model.Attestation) error { verifyCalled = true; return nil })
	require.ErrorIs(t, err, model.ErrUnattestedCommit)
	assert.Nil(t, sid, "a rejected commit records no provenance")
	assert.False(t, verifyCalled, "no attestation means nothing to verify")
}

// TestRequireSandbox_OffRecordsNullSandboxID verifies that with the
// policy off, an unattested commit lands with sandbox_id = NULL — the
// permanently visible audit gap. Refs: FR-17.6, F-02, SEC-02
func TestRequireSandbox_OffRecordsNullSandboxID(t *testing.T) {
	sid, err := EnforceRequireSandbox(context.Background(), false, nil,
		func(context.Context, *model.Attestation) error { return assert.AnError })
	require.NoError(t, err)
	assert.Nil(t, sid, "policy off → NULL sandbox_id (the visible gap)")
}

// TestRequireSandbox_DefaultTrueSafetyCritical verifies the safe default:
// require_sandbox is on unless explicitly disabled. Refs: FR-17.6
func TestRequireSandbox_DefaultTrueSafetyCritical(t *testing.T) {
	assert.True(t, model.DefaultSandboxPolicy().RequireSandbox,
		"require_sandbox must default to true in the safety-critical profile")
}

// TestRequireSandbox_ValidAttestation_RecordsProvenance verifies a valid
// host attestation under the policy lands with its sandbox_id, using the
// REAL host attestor. Refs: FR-17.6, SEC-01
func TestRequireSandbox_ValidAttestation_RecordsProvenance(t *testing.T) {
	svc, att := realAttestor(t)
	sid, err := EnforceRequireSandbox(context.Background(), true, att, svc.Verify)
	require.NoError(t, err)
	require.NotNil(t, sid)
	assert.Equal(t, att.SandboxID, *sid, "the attested sandbox is recorded as provenance")
}

// TestRequireSandbox_ForgedAttestation_Rejected verifies that under the
// policy a present-but-invalid attestation is refused (not silently
// accepted), using the real attestor. Refs: SEC-01
func TestRequireSandbox_ForgedAttestation_Rejected(t *testing.T) {
	svc, att := realAttestor(t)
	forged := *att
	forged.SandboxID = "01JXSBFORGED0000000000000000" // tamper a signed field
	sid, err := EnforceRequireSandbox(context.Background(), true, &forged, svc.Verify)
	require.ErrorIs(t, err, model.ErrAttestationInvalid)
	assert.Nil(t, sid)
}
