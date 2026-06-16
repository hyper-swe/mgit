// Package attest tests verify host-anchored commit attestation (SEC-01)
// and its key management (FR-17.38). The guest is the hostile party and
// holds no key; only the host can issue an attestation that verifies.
// Refs: FR-17.6, FR-17.38, MGIT-11.8.1
package attest

import (
	"context"
	"crypto/ed25519"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/hyper-swe/mgit/internal/model"
)

const (
	testSandbox = "01JXSBTESTSANDBOX0000000000"
	testCommit  = "0123456789abcdef0123456789abcdef01234567"                         // 40 hex
	testContent = "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef" // 64 hex
)

// recorder captures key-lifecycle audit events.
type recorder struct{ details []string }

func (r *recorder) RecordKeyChange(_ context.Context, detail string) error {
	r.details = append(r.details, detail)
	return nil
}

func fixedClock() func() time.Time {
	return func() time.Time { return time.Date(2026, 6, 16, 12, 0, 0, 123, time.UTC) }
}

// newService generates a key under a fresh host root and opens a service.
func newService(t *testing.T) (*Service, string, *recorder) {
	t.Helper()
	hostRoot := t.TempDir()
	rec := &recorder{}
	require.NoError(t, GenerateKey(context.Background(), hostRoot, rec))
	svc, err := NewService(hostRoot, fixedClock())
	require.NoError(t, err)
	return svc, hostRoot, rec
}

// TestAttest_IssuedHostSide verifies the host issues an attestation that
// verifies against host key material. Refs: SEC-01, FR-17.6
func TestAttest_IssuedHostSide(t *testing.T) {
	svc, _, _ := newService(t)
	att, err := svc.Attest(context.Background(), testSandbox, testCommit, testContent)
	require.NoError(t, err)
	require.NoError(t, att.Validate())
	assert.Equal(t, model.AlgEd25519, att.Alg)
	assert.NotEmpty(t, att.KeyID)
	assert.Equal(t, testSandbox, att.SandboxID)
	assert.NoError(t, svc.Verify(context.Background(), att), "the host-issued attestation must verify")
}

// TestAttest_GuestCannotForge_NoKey verifies that without the host
// private key an attestation cannot be forged: a signature from a
// different (guest-held) key, and an empty/garbage signature, both fail
// verification. Refs: SEC-01
func TestAttest_GuestCannotForge_NoKey(t *testing.T) {
	svc, _, _ := newService(t)
	good, err := svc.Attest(context.Background(), testSandbox, testCommit, testContent)
	require.NoError(t, err)

	// A guest forging with its own key (same key_id claim) must fail:
	// it does not hold the host private key.
	_, guestPriv, err := ed25519.GenerateKey(nil)
	require.NoError(t, err)
	forged := *good
	forged.HostSignature = ed25519.Sign(guestPriv, []byte("anything"))
	assert.ErrorIs(t, svc.Verify(context.Background(), &forged), model.ErrAttestationInvalid,
		"a signature not from the host key must be rejected")

	// Garbage / empty signatures fail.
	forged2 := *good
	forged2.HostSignature = []byte("not-a-signature")
	assert.Error(t, svc.Verify(context.Background(), &forged2))
}

// TestAttest_BindsCommitAndSandbox verifies the signature binds every
// field: tampering with sandbox, commit, or content after issuance
// fails verification. Refs: SEC-01, FR-17.6
func TestAttest_BindsCommitAndSandbox(t *testing.T) {
	svc, _, _ := newService(t)
	att, err := svc.Attest(context.Background(), testSandbox, testCommit, testContent)
	require.NoError(t, err)

	otherCommit := strings.Repeat("a", 40)
	otherContent := strings.Repeat("b", 64)
	for name, mutate := range map[string]func(*model.Attestation){
		"sandbox_swapped": func(a *model.Attestation) { a.SandboxID = "01JXSBOTHER000000000000000" },
		"commit_swapped":  func(a *model.Attestation) { a.CommitHash = otherCommit },
		"content_swapped": func(a *model.Attestation) { a.ContentHash = otherContent },
		"issued_at_moved": func(a *model.Attestation) { a.IssuedAt = a.IssuedAt.Add(time.Second) },
		"key_id_swapped":  func(a *model.Attestation) { a.KeyID = strings.Repeat("0", 64) },
	} {
		t.Run(name, func(t *testing.T) {
			tampered := *att
			mutate(&tampered)
			assert.Error(t, svc.Verify(context.Background(), &tampered),
				"tampering with %s must fail verification", name)
		})
	}
}

// TestAttest_KeyStoredOwnerOnlySeparateFile verifies FR-17.38 key
// management: the attestation key is its own 0600 file, separate from
// the image-signing trust root, policy store, and images.lock.
func TestAttest_KeyStoredOwnerOnlySeparateFile(t *testing.T) {
	_, hostRoot, _ := newService(t)
	keyPath := filepath.Join(hostRoot, "trust", attestKeyFile)
	info, err := os.Stat(keyPath)
	require.NoError(t, err, "attestation private key must exist under the host trust dir")
	assert.NotEqual(t, keyPath, filepath.Join(hostRoot, "trust", "image-signing.key"),
		"attestation key must be a separate file from the image-signing key")
	if runtime.GOOS != "windows" {
		assert.Equal(t, os.FileMode(0o600), info.Mode().Perm(), "private key must be owner-only (FR-17.38)")
	}
}

// TestAttest_RotationAuditsFingerprints verifies rotation appends an
// audit event with old and new fingerprints, and that an attestation
// issued under the old key still verifies (key_id selects the key).
// Refs: FR-17.38
func TestAttest_RotationAuditsFingerprints(t *testing.T) {
	svc, hostRoot, rec := newService(t)
	oldAtt, err := svc.Attest(context.Background(), testSandbox, testCommit, testContent)
	require.NoError(t, err)

	require.NoError(t, GenerateKey(context.Background(), hostRoot, rec)) // rotate
	require.Len(t, rec.details, 2, "first generation + rotation are both audited")
	assert.Contains(t, rec.details[1], "old_fingerprint")
	assert.Contains(t, rec.details[1], "new_fingerprint")

	rotated, err := NewService(hostRoot, fixedClock())
	require.NoError(t, err)
	assert.NoError(t, rotated.Verify(context.Background(), oldAtt),
		"an attestation under a rotated-out key must still verify via key_id")

	newAtt, err := rotated.Attest(context.Background(), testSandbox, testCommit, testContent)
	require.NoError(t, err)
	assert.NotEqual(t, oldAtt.KeyID, newAtt.KeyID, "rotation changes the active key_id")
}

// TestNewService_NoKey_FailsClosed verifies the service refuses to open
// without a generated key (no silent unkeyed attestor). Refs: SEC-01
func TestNewService_NoKey_FailsClosed(t *testing.T) {
	_, err := NewService(t.TempDir(), fixedClock())
	require.Error(t, err)
}

// TestAttest_RejectsMalformedInput verifies Attest validates its inputs
// (no signing oracle for garbage hashes).
func TestAttest_RejectsMalformedInput(t *testing.T) {
	svc, _, _ := newService(t)
	_, err := svc.Attest(context.Background(), "", testCommit, testContent)
	require.Error(t, err)
	_, err = svc.Attest(context.Background(), testSandbox, "nothex", testContent)
	require.Error(t, err)
}

// failRecorder fails to record, exercising the audit-error path.
type failRecorder struct{}

func (failRecorder) RecordKeyChange(context.Context, string) error {
	return assert.AnError
}

// TestGenerateKey_InvalidArgs covers the fail-closed guards.
func TestGenerateKey_InvalidArgs(t *testing.T) {
	require.Error(t, GenerateKey(context.Background(), "", &recorder{}), "empty host root")
	require.Error(t, GenerateKey(context.Background(), t.TempDir(), nil), "nil auditor")
	require.Error(t, GenerateKey(context.Background(), t.TempDir(), failRecorder{}), "audit failure aborts")
}

// TestNewService_InvalidInputs covers the clock guard and a corrupt key.
func TestNewService_InvalidInputs(t *testing.T) {
	hostRoot := t.TempDir()
	require.NoError(t, GenerateKey(context.Background(), hostRoot, &recorder{}))

	_, err := NewService(hostRoot, nil)
	require.Error(t, err, "nil clock rejected")

	// Corrupt the private key to a wrong size.
	keyPath := filepath.Join(hostRoot, "trust", attestKeyFile)
	require.NoError(t, os.WriteFile(keyPath, []byte("too-short"), 0o600))
	_, err = NewService(hostRoot, fixedClock())
	require.Error(t, err, "a malformed key fails closed")
}

// TestGenerateKey_WriteError verifies key generation fails closed when
// the trust dir cannot be written (the key must never be half-created).
// Skipped as root (root bypasses dir perms) and on Windows (POSIX modes).
func TestGenerateKey_WriteError(t *testing.T) {
	if os.Geteuid() == 0 || runtime.GOOS == "windows" {
		t.Skip("requires POSIX dir permissions and non-root")
	}
	hostRoot := t.TempDir()
	dir := filepath.Join(hostRoot, "trust")
	require.NoError(t, os.MkdirAll(dir, 0o500)) // readable+executable, not writable
	t.Cleanup(func() { _ = os.Chmod(dir, 0o700) })

	require.Error(t, GenerateKey(context.Background(), hostRoot, &recorder{}),
		"an unwritable trust dir must fail closed, not half-create a key")
}

// TestGenerateKey_RotationRetainError verifies a rotation that cannot
// retain the prior public key fails closed rather than discarding it.
func TestGenerateKey_RotationRetainError(t *testing.T) {
	hostRoot := t.TempDir()
	require.NoError(t, GenerateKey(context.Background(), hostRoot, &recorder{}))
	// Block the rotated-key dir by occupying its path with a file.
	require.NoError(t, os.WriteFile(filepath.Join(hostRoot, "trust", rotatedPubDir), []byte("x"), 0o600))
	require.Error(t, GenerateKey(context.Background(), hostRoot, &recorder{}),
		"rotation must fail if the prior key cannot be retained")
}

// TestGenerateKey_KeyPathOccupied verifies generation fails closed when
// the key path is occupied by a directory (the atomic rename cannot
// complete), exercising the write→close→rename path.
func TestGenerateKey_KeyPathOccupied(t *testing.T) {
	hostRoot := t.TempDir()
	require.NoError(t, os.MkdirAll(filepath.Join(hostRoot, "trust", attestKeyFile), 0o700))
	require.Error(t, GenerateKey(context.Background(), hostRoot, &recorder{}),
		"a key path occupied by a directory must fail closed")
}

// TestVerify_NilAndBadAlg covers the early Verify rejections.
func TestVerify_NilAndBadAlg(t *testing.T) {
	svc, _, _ := newService(t)
	assert.ErrorIs(t, svc.Verify(context.Background(), nil), model.ErrAttestationInvalid)

	att, err := svc.Attest(context.Background(), testSandbox, testCommit, testContent)
	require.NoError(t, err)
	badAlg := *att
	badAlg.Alg = "rsa"
	assert.ErrorIs(t, svc.Verify(context.Background(), &badAlg), model.ErrAttestationInvalid)

	hollow := &model.Attestation{} // fails structural Validate
	assert.ErrorIs(t, svc.Verify(context.Background(), hollow), model.ErrAttestationInvalid)
}
