package images

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"fmt"
	"os"
	"path/filepath"

	"github.com/hyper-swe/mgit/internal/sandboxd/hostkey"
)

// Trust-root file layout under the host config root. The signing key
// is owner-only and lives in its own directory, SEPARATE from
// images.lock (FR-17.38): a writer who can poison the lock must not be
// able to substitute the verification key.
const (
	signingKey = "image-signing.key" // private key, 0600
	signingPub = "image-signing.pub" // public verification key
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
	dir := filepath.Join(hostRoot, hostkey.TrustDirName)
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
	if err := hostkey.WriteFileAtomic(filepath.Join(dir, signingKey), priv); err != nil {
		return nil, fmt.Errorf("trust root: write key: %w", err)
	}
	if err := hostkey.WriteFileAtomic(filepath.Join(dir, signingPub), pub); err != nil {
		return nil, fmt.Errorf("trust root: write pub: %w", err)
	}

	newFingerprint := hostkey.Fingerprint(pub)
	detail := fmt.Sprintf(`{"new_fingerprint":%q}`, newFingerprint)
	if oldFingerprint != "" {
		detail = fmt.Sprintf(`{"old_fingerprint":%q,"new_fingerprint":%q}`, oldFingerprint, newFingerprint)
	}
	if err := audit.RecordTrustRootChange(ctx, detail); err != nil {
		return nil, fmt.Errorf("trust root: record change: %w", err)
	}
	return priv, nil
}

// loadTrustRoot reads the public verification key. Absence (or an
// invalid key) is fatal: without it nothing can be verified (fail closed).
func loadTrustRoot(hostRoot string) (ed25519.PublicKey, error) {
	pub := hostkey.ReadPub(filepath.Join(hostRoot, hostkey.TrustDirName, signingPub))
	if pub == nil {
		return nil, fmt.Errorf("images: load trust root (run GenerateTrustRoot first): missing or invalid key")
	}
	return pub, nil
}

// existingFingerprint returns the fingerprint of the current public
// key, or "" if none exists yet.
func existingFingerprint(dir string) string {
	if pub := hostkey.ReadPub(filepath.Join(dir, signingPub)); pub != nil {
		return hostkey.Fingerprint(pub)
	}
	return ""
}
