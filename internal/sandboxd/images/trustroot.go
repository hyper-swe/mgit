package images

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
)

// Trust-root file layout under the host config root. The signing key
// is owner-only and lives in its own directory, SEPARATE from
// images.lock (FR-17.38): a writer who can poison the lock must not be
// able to substitute the verification key.
const (
	trustDirName = "trust"
	signingKey   = "image-signing.key" // private key, 0600
	signingPub   = "image-signing.pub" // public verification key
)

// TrustRootAuditor records trust-root lifecycle events (FR-17.38).
type TrustRootAuditor interface {
	RecordTrustRootChange(ctx context.Context, detail string) error
}

// GenerateTrustRoot creates (or rotates) the image-signing trust root
// under hostRoot/trust and records the change. On first generation the
// audit detail carries the new fingerprint; on rotation it carries
// both the replaced and the new fingerprint. Returns the private key
// so the host signing tool can sign images.lock entries. The private
// key is written 0600; rotation overwrites the prior key.
// Refs: FR-17.38
func GenerateTrustRoot(ctx context.Context, hostRoot string, audit TrustRootAuditor) (ed25519.PrivateKey, error) {
	if hostRoot == "" {
		return nil, fmt.Errorf("trust root: host root must not be empty")
	}
	if audit == nil {
		return nil, fmt.Errorf("trust root: auditor must not be nil")
	}
	dir := filepath.Join(hostRoot, trustDirName)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, fmt.Errorf("trust root: create dir: %w", err)
	}

	oldFingerprint := existingFingerprint(dir)

	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return nil, fmt.Errorf("trust root: generate key: %w", err)
	}
	// Atomic writes (temp + rename): a crash mid-write never leaves a
	// torn key on disk, and a reader never observes a half-written key.
	if err := writeFileAtomic(filepath.Join(dir, signingKey), priv); err != nil {
		return nil, fmt.Errorf("trust root: write key: %w", err)
	}
	if err := writeFileAtomic(filepath.Join(dir, signingPub), pub); err != nil {
		return nil, fmt.Errorf("trust root: write pub: %w", err)
	}

	newFingerprint := fingerprint(pub)
	detail := fmt.Sprintf(`{"new_fingerprint":%q}`, newFingerprint)
	if oldFingerprint != "" {
		detail = fmt.Sprintf(`{"old_fingerprint":%q,"new_fingerprint":%q}`, oldFingerprint, newFingerprint)
	}
	if err := audit.RecordTrustRootChange(ctx, detail); err != nil {
		return nil, fmt.Errorf("trust root: record change: %w", err)
	}
	return priv, nil
}

// loadTrustRoot reads the public verification key. Absence is fatal:
// without it nothing can be verified (fail closed).
func loadTrustRoot(hostRoot string) (ed25519.PublicKey, error) {
	data, err := os.ReadFile(filepath.Join(hostRoot, trustDirName, signingPub)) //nolint:gosec // host-owned config path
	if err != nil {
		return nil, fmt.Errorf("images: load trust root (run GenerateTrustRoot first): %w", err)
	}
	if len(data) != ed25519.PublicKeySize {
		return nil, fmt.Errorf("images: trust root key is not a valid Ed25519 public key")
	}
	return ed25519.PublicKey(data), nil
}

// existingFingerprint returns the fingerprint of the current public
// key, or "" if none exists yet.
func existingFingerprint(dir string) string {
	data, err := os.ReadFile(filepath.Join(dir, signingPub)) //nolint:gosec // host-owned config path
	if err != nil || len(data) != ed25519.PublicKeySize {
		return ""
	}
	return fingerprint(ed25519.PublicKey(data))
}

// fingerprint is the hex SHA-256 of a public key.
func fingerprint(pub ed25519.PublicKey) string {
	sum := sha256.Sum256(pub)
	return hex.EncodeToString(sum[:])
}

// writeFileAtomic writes data to a temp file in the same directory and
// renames it into place (0600), so the destination is never torn.
func writeFileAtomic(path string, data []byte) error {
	tmp, err := os.CreateTemp(filepath.Dir(path), ".tmp-*")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	defer func() { _ = os.Remove(tmpName) }() // no-op once renamed

	if err := tmp.Chmod(0o600); err != nil {
		_ = tmp.Close()
		return err
	}
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmpName, path)
}
