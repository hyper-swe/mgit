// Package attest implements host-anchored commit attestation (SEC-01,
// FR-17.6). mgit-sandboxd issues an attestation for each commit it
// observes crossing a sandbox's land channel, signed with a host-held
// Ed25519 key the guest never sees; the guest (mgit-guest) holds no key
// and cannot forge one. Verification binds every field via a
// byte-stable, length-prefixed signing payload (IDD §3.3) and selects
// the public key by key_id so attestations survive key rotation
// (FR-17.38). Host-side only; pure Go (stdlib crypto). Refs: FR-17.6,
// FR-17.38, SEC-01, MGIT-11.8.1
package attest

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/binary"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/hyper-swe/mgit/internal/model"
	"github.com/hyper-swe/mgit/internal/sandboxd/hostkey"
)

// Key file layout under the host config root. The attestation key is
// owner-only (0600) and a SEPARATE file from the image-signing trust
// root, the policy store, and images.lock (FR-17.38): one compromised
// or poisoned artifact must not substitute another's verification key.
const (
	attestKeyFile = "attestation-signing.key" // private key, 0600
	attestPubFile = "attestation-signing.pub" // active public key
	rotatedPubDir = "attestation-rotated"     // retired public keys, by fingerprint
)

// KeyAuditor records attestation-key lifecycle events (FR-17.38).
type KeyAuditor interface {
	RecordKeyChange(ctx context.Context, detail string) error
}

// GenerateKey creates (or rotates) the host attestation key under
// hostRoot/trust and records the change. On rotation the prior public
// key is retained (under rotatedPubDir, keyed by fingerprint) so
// attestations issued under it still verify, and the audit detail
// carries both fingerprints. The private key is written 0600. Refs: FR-17.38
func GenerateKey(ctx context.Context, hostRoot string, audit KeyAuditor) error {
	if hostRoot == "" {
		return fmt.Errorf("attest: host root must not be empty")
	}
	if audit == nil {
		return fmt.Errorf("attest: auditor must not be nil")
	}
	dir := filepath.Join(hostRoot, hostkey.TrustDirName)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("attest: create trust dir: %w", err)
	}

	oldPub := hostkey.ReadPub(filepath.Join(dir, attestPubFile))
	if oldPub != nil {
		// Retain the retired public key so its key_id keeps verifying.
		if err := os.MkdirAll(filepath.Join(dir, rotatedPubDir), 0o700); err != nil {
			return fmt.Errorf("attest: create rotated dir: %w", err)
		}
		if err := hostkey.WriteFileAtomic(filepath.Join(dir, rotatedPubDir, hostkey.Fingerprint(oldPub)), oldPub); err != nil {
			return fmt.Errorf("attest: retain rotated key: %w", err)
		}
	}

	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return fmt.Errorf("attest: generate key: %w", err)
	}
	if err := hostkey.WriteFileAtomic(filepath.Join(dir, attestKeyFile), priv); err != nil {
		return fmt.Errorf("attest: write key: %w", err)
	}
	if err := hostkey.WriteFileAtomic(filepath.Join(dir, attestPubFile), pub); err != nil {
		return fmt.Errorf("attest: write pub: %w", err)
	}

	detail := fmt.Sprintf(`{"event":"attestation_key","new_fingerprint":%q}`, hostkey.Fingerprint(pub))
	if oldPub != nil {
		detail = fmt.Sprintf(`{"event":"attestation_key_rotated","old_fingerprint":%q,"new_fingerprint":%q}`,
			hostkey.Fingerprint(oldPub), hostkey.Fingerprint(pub))
	}
	if err := audit.RecordKeyChange(ctx, detail); err != nil {
		return fmt.Errorf("attest: record key change: %w", err)
	}
	return nil
}

// Service is the host-side Attestor. It holds the active signing key and
// every known public key (active + rotated) so it can verify by key_id.
type Service struct {
	priv      ed25519.PrivateKey
	activeKey string // fingerprint of the active public key
	knownPub  map[string]ed25519.PublicKey
	clock     func() time.Time
}

// NewService loads the host attestation key material. Absence is fatal
// (fail closed): an unkeyed daemon must never silently issue or accept
// unverifiable attestations. Refs: SEC-01, FR-17.38
func NewService(hostRoot string, clock func() time.Time) (*Service, error) {
	if clock == nil {
		return nil, fmt.Errorf("attest: clock must not be nil")
	}
	dir := filepath.Join(hostRoot, hostkey.TrustDirName)
	privBytes, err := os.ReadFile(filepath.Join(dir, attestKeyFile)) //nolint:gosec // host-owned config path
	if err != nil {
		return nil, fmt.Errorf("attest: load signing key (run GenerateKey first): %w", err)
	}
	if len(privBytes) != ed25519.PrivateKeySize {
		return nil, fmt.Errorf("attest: signing key is not a valid Ed25519 private key")
	}
	priv := ed25519.PrivateKey(privBytes)
	pub, ok := priv.Public().(ed25519.PublicKey)
	if !ok {
		return nil, fmt.Errorf("attest: signing key has no Ed25519 public half")
	}

	activeKey := hostkey.Fingerprint(pub)
	known := map[string]ed25519.PublicKey{activeKey: pub}
	// Load any retired public keys so their attestations still verify.
	entries, _ := os.ReadDir(filepath.Join(dir, rotatedPubDir))
	for _, e := range entries {
		if p := hostkey.ReadPub(filepath.Join(dir, rotatedPubDir, e.Name())); p != nil {
			known[hostkey.Fingerprint(p)] = p
		}
	}
	return &Service{priv: priv, activeKey: activeKey, knownPub: known, clock: clock}, nil
}

// Attest issues an attestation for one observed commit. The caller
// (the land flow) MUST pass hashes it computed from bytes it itself read
// hash-on-write — Attest signs what it is given, so it must never be fed
// guest-asserted hashes (SEC-01, IDD §3.1). Refs: FR-17.6
func (s *Service) Attest(_ context.Context, sandboxID, commitHash, contentHash string) (*model.Attestation, error) {
	att := &model.Attestation{
		SandboxID:   sandboxID,
		CommitHash:  commitHash,
		ContentHash: contentHash,
		Alg:         model.AlgEd25519,
		KeyID:       s.activeKey,
		IssuedAt:    s.clock().UTC(),
	}
	att.HostSignature = ed25519.Sign(s.priv, signingPayload(att))
	// Validate the fully-formed attestation; a malformed (sandboxID,
	// hash) input is a host-side programming error, caught here before
	// the attestation is returned (the signature, computed over bad
	// fields, never leaves on the error path). Not a guest-facing oracle.
	if err := att.Validate(); err != nil {
		return nil, fmt.Errorf("attest: %w", err)
	}
	return att, nil
}

// Verify checks an attestation per IDD §3.4: structural shape, the only
// permitted algorithm, a known key_id, then the Ed25519 signature over
// the recomputed canonical payload. Any tampered field fails.
// Refs: FR-17.6, SEC-01
func (s *Service) Verify(_ context.Context, att *model.Attestation) error {
	if att == nil {
		return fmt.Errorf("%w: nil attestation", model.ErrAttestationInvalid)
	}
	if err := att.Validate(); err != nil {
		return fmt.Errorf("%w: %w", model.ErrAttestationInvalid, err)
	}
	if att.Alg != model.AlgEd25519 {
		return fmt.Errorf("%w: unsupported algorithm %q", model.ErrAttestationInvalid, att.Alg)
	}
	pub, ok := s.knownPub[att.KeyID]
	if !ok {
		return fmt.Errorf("%w: unknown key_id %q", model.ErrAttestationInvalid, att.KeyID)
	}
	if !ed25519.Verify(pub, signingPayload(att), att.HostSignature) {
		return fmt.Errorf("%w: signature does not verify", model.ErrAttestationInvalid)
	}
	return nil
}

// signingPayload builds the byte-stable, length-prefixed canonical
// signing input (IDD §3.3): sandbox_id, commit_hash, content_hash,
// key_id (UTF-8), then issued_at as the raw 8 big-endian bytes of
// UnixNano — never re-serialized JSON or RFC3339. Length prefixing rules
// out field-boundary collisions; binding key_id rules out key/alg
// confusion. Refs: FR-17.38
func signingPayload(a *model.Attestation) []byte {
	var buf bytes.Buffer
	for _, field := range []string{a.SandboxID, a.CommitHash, a.ContentHash, a.KeyID} {
		var length [8]byte
		binary.BigEndian.PutUint64(length[:], uint64(len(field)))
		buf.Write(length[:])
		buf.WriteString(field)
	}
	var nano [8]byte
	binary.BigEndian.PutUint64(nano[:], uint64(a.IssuedAt.UTC().UnixNano())) //nolint:gosec // signed/unsigned bit pattern is stable and only used as signed input
	buf.Write(nano[:])
	return buf.Bytes()
}
