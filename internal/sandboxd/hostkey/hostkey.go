// Package hostkey holds the shared primitives for host-side trust-anchor
// key material under the host config root: the trust-directory layout,
// atomic owner-only file writes, Ed25519 public-key reads, and key
// fingerprints. Both the image-signing trust root (FR-17.29/38) and the
// commit-attestation key (FR-17.6/38) persist Ed25519 keys the same way;
// keeping that in one place gives a single reviewable owner for how
// security-critical keys hit disk, so a hardening change (e.g. adding
// fsync) lands once rather than drifting across copies. Host-side, pure
// Go. Refs: FR-17.38, SEC-01
package hostkey

import (
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/hex"
	"io"
	"os"
	"path/filepath"
)

// TrustDirName is the host config sub-directory holding trust-anchor key
// material (separate files per purpose, FR-17.38).
const TrustDirName = "trust"

// Fingerprint is the key_id for a public key: hex SHA-256 of the key
// bytes. Used as the attestation key_id (IDD §3.5) and in trust-root
// rotation audit records.
func Fingerprint(pub ed25519.PublicKey) string {
	sum := sha256.Sum256(pub)
	return hex.EncodeToString(sum[:])
}

// ReadPub reads an Ed25519 public key file, returning nil if it is
// absent or not a valid public-key size (callers treat nil as "no key").
func ReadPub(path string) ed25519.PublicKey {
	data, err := os.ReadFile(path) //nolint:gosec // host-owned config path
	if err != nil || len(data) != ed25519.PublicKeySize {
		return nil
	}
	return ed25519.PublicKey(data)
}

// tempFile is the subset of *os.File WriteFileAtomic needs. It exists so
// the fail-closed write/close error paths are testable by injection — a
// torn or failed key write MUST surface, never silently succeed.
type tempFile interface {
	io.Writer
	io.Closer
	Name() string
}

// createTemp opens the staging temp file. A package var (effectively
// const in production) only so fault injection can exercise the
// fail-closed paths; never reassigned outside tests.
var createTemp = func(dir string) (tempFile, error) { return os.CreateTemp(dir, ".tmp-*") }

// WriteFileAtomic writes data to a temp file in the destination
// directory and renames it into place, so the destination is never
// observed torn (a crash mid-write leaves only the temp file). The file
// is owner-only (0600): os.CreateTemp already creates 0600-before-umask,
// and umask only clears bits, so the result is owner-rw at most — no
// explicit chmod needed. Intended for small key material.
func WriteFileAtomic(path string, data []byte) error {
	tmp, err := createTemp(filepath.Dir(path))
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	defer func() { _ = os.Remove(tmpName) }() // no-op once renamed
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmpName, path)
}
