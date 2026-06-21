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

// matchingCommit is the commit an attestation was issued for (so the
// anti-replay binding check passes).
func matchingCommit(att *model.Attestation) *model.Commit {
	return &model.Commit{CommitID: att.CommitHash, ContentHash: att.ContentHash}
}

// TestRequireSandbox_UnattestedCommit_Rejected verifies that with the
// policy on, a commit without an attestation is refused. Refs: FR-17.6, F-02
func TestRequireSandbox_UnattestedCommit_Rejected(t *testing.T) {
	verifyCalled := false
	sid, err := EnforceRequireSandbox(context.Background(), true, "01JXSBLAND00000000000000000",
		&model.Commit{CommitID: "1111111111111111111111111111111111111111"}, nil,
		func(context.Context, *model.Attestation) error { verifyCalled = true; return nil })
	require.ErrorIs(t, err, model.ErrUnattestedCommit)
	assert.Nil(t, sid, "a rejected commit records no provenance")
	assert.False(t, verifyCalled, "no attestation means nothing to verify")
}

// TestRequireSandbox_OffRecordsNullSandboxID verifies that with the
// policy off, an unattested commit lands with sandbox_id = NULL — the
// permanently visible audit gap. Refs: FR-17.6, F-02, SEC-02
func TestRequireSandbox_OffRecordsNullSandboxID(t *testing.T) {
	sid, err := EnforceRequireSandbox(context.Background(), false, "", nil, nil,
		func(context.Context, *model.Attestation) error { return assert.AnError })
	require.NoError(t, err)
	assert.Nil(t, sid, "policy off → NULL sandbox_id (the visible gap)")
}

// TestRequireSandbox_NilCommit_FailsClosed verifies the gate refuses
// when the policy is on but no commit is supplied (defensive guard).
func TestRequireSandbox_NilCommit_FailsClosed(t *testing.T) {
	_, err := EnforceRequireSandbox(context.Background(), true, "", nil, nil,
		func(context.Context, *model.Attestation) error { return nil })
	require.ErrorIs(t, err, model.ErrLandVerificationFailed)
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
	sid, err := EnforceRequireSandbox(context.Background(), true, att.SandboxID, matchingCommit(att), att, svc.Verify)
	require.NoError(t, err)
	require.NotNil(t, sid)
	assert.Equal(t, att.SandboxID, *sid, "the attested sandbox is recorded as provenance")
}

// TestRequireSandbox_ReplayedAttestation_Rejected verifies an authentic
// attestation captured from one commit cannot land a DIFFERENT commit
// (anti-replay). Refs: SEC-01, F-02
func TestRequireSandbox_ReplayedAttestation_Rejected(t *testing.T) {
	svc, att := realAttestor(t) // a valid attestation for commit X
	// The guest replays it to land a different commit Y.
	otherCommit := &model.Commit{
		CommitID:    "9999999999999999999999999999999999999999",
		ContentHash: att.ContentHash,
	}
	sid, err := EnforceRequireSandbox(context.Background(), true, att.SandboxID, otherCommit, att, svc.Verify)
	require.ErrorIs(t, err, model.ErrAttestationInvalid,
		"an authentic attestation for another commit must not land this one")
	assert.Nil(t, sid)

	// Also reject when only the content_hash differs.
	otherContent := &model.Commit{
		CommitID:    att.CommitHash,
		ContentHash: "00000000000000000000000000000000000000000000000000000000deadbeef",
	}
	_, err = EnforceRequireSandbox(context.Background(), true, att.SandboxID, otherContent, att, svc.Verify)
	assert.ErrorIs(t, err, model.ErrAttestationInvalid)
}

// TestRequireSandbox_WrongSandbox_Rejected verifies an authentic attestation
// issued for a DIFFERENT sandbox (same host key, matching commit hashes)
// cannot stamp this land's provenance — it must name the bound sandbox.
// Refs: SEC-01, SEC-10
func TestRequireSandbox_WrongSandbox_Rejected(t *testing.T) {
	svc, att := realAttestor(t) // attestation bound to the land sandbox id
	sid, err := EnforceRequireSandbox(context.Background(), true,
		"01JXSBOTHER0000000000000000", matchingCommit(att), att, svc.Verify)
	require.ErrorIs(t, err, model.ErrAttestationInvalid,
		"an attestation for another sandbox must not land this one")
	assert.Nil(t, sid)
}

// TestRequireSandbox_ForgedAttestation_Rejected verifies that under the
// policy a present-but-invalid attestation is refused (not silently
// accepted), using the real attestor. Refs: SEC-01
func TestRequireSandbox_ForgedAttestation_Rejected(t *testing.T) {
	svc, att := realAttestor(t)
	forged := *att
	forged.SandboxID = "01JXSBFORGED0000000000000000" // tamper a signed field
	sid, err := EnforceRequireSandbox(context.Background(), true, att.SandboxID, matchingCommit(att), &forged, svc.Verify)
	require.ErrorIs(t, err, model.ErrAttestationInvalid)
	assert.Nil(t, sid)
}
