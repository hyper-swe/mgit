// Package images resolves digest-pinned guest images from a host-owned
// images.lock and verifies them at boot: the pinned content digest
// must match the image bytes (FR-17.17) AND a detached Ed25519
// signature over that digest must verify against the host trust root
// (SEC-12/F-10/FR-17.29). The trust root lives in a file separate from
// images.lock so a lock-writer cannot substitute the verification key
// (FR-17.38). The warm pool (warmpool.go) snapshots only a clean base
// boot (SEC-08/FR-17.25). Refs: FR-17.17, FR-17.29, FR-17.38
package images

import (
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/hyper-swe/mgit/internal/model"
)

// lockFileName is the host-owned image pin file (FR-17.13, FR-17.36).
const lockFileName = "images.lock"

// Entry is one pinned, signed guest image. Signature is a detached
// Ed25519 signature over Digest, verified against the trust root.
type Entry struct {
	Digest     string `json:"digest"`      // sha256:<hex> of the rootfs
	KernelPath string `json:"kernel_path"` // host path to the guest kernel
	RootfsPath string `json:"rootfs_path"` // host path to the read-only rootfs
	Cmdline    string `json:"cmdline"`     // guest kernel command line
	Signature  []byte `json:"signature"`   // Ed25519(trust_root, digest)
}

// Lock is the images.lock document: image name -> pinned entry.
type Lock struct {
	Images map[string]Entry `json:"images"`
}

// ResolvedImage is a verified image's on-disk locations.
type ResolvedImage struct {
	KernelPath string
	RootfsPath string
	Cmdline    string
}

// Store resolves images against a host config root, verifying digest
// and signature on every Resolve. images.lock is re-read each call (a
// rewritten lock takes effect immediately); the trust-root public key
// is loaded once at construction.
type Store struct {
	hostRoot string
	clock    func() time.Time
	trustPub ed25519.PublicKey
}

// NewStore opens a Store rooted at hostRoot. It requires the trust
// root (GenerateTrustRoot must have run): without a verification key,
// no image can be verified, so the store fails closed.
func NewStore(hostRoot string, clock func() time.Time) (*Store, error) {
	if hostRoot == "" {
		return nil, fmt.Errorf("images: host root must not be empty")
	}
	if clock == nil {
		return nil, fmt.Errorf("images: clock must not be nil")
	}
	pub, err := loadTrustRoot(hostRoot)
	if err != nil {
		return nil, err
	}
	return &Store{hostRoot: hostRoot, clock: clock, trustPub: pub}, nil
}

// Resolve verifies and locates the image named by a digest-pinned
// reference (<name>@sha256:<hex>). It refuses (ErrVerificationFailed)
// when the reference digest disagrees with the lock, the signature
// does not verify against the trust root, or the rootfs content does
// not hash to the pinned digest. Refs: FR-17.17, FR-17.29
func (s *Store) Resolve(imageRef string) (ResolvedImage, error) {
	if err := model.ValidateImageRef(imageRef); err != nil {
		return ResolvedImage{}, fmt.Errorf("images resolve: %w", err)
	}
	name, refDigest, _ := strings.Cut(imageRef, "@")

	lock, err := s.readLock()
	if err != nil {
		return ResolvedImage{}, err
	}
	entry, ok := lock.Images[name]
	if !ok {
		return ResolvedImage{}, fmt.Errorf("images resolve: %q not in %s", name, lockFileName)
	}

	// The reference must pin the same digest the lock records.
	if entry.Digest != refDigest {
		return ResolvedImage{}, fmt.Errorf("%w: ref digest %s != lock digest %s",
			model.ErrVerificationFailed, refDigest, entry.Digest)
	}
	// The signature must verify against the host trust root (SEC-12):
	// detects a poisoned lock entry, not just a tampered image.
	if !ed25519.Verify(s.trustPub, []byte(entry.Digest), entry.Signature) {
		return ResolvedImage{}, fmt.Errorf("%w: image %q signature does not verify against the trust root",
			model.ErrVerificationFailed, name)
	}
	// The rootfs content must hash to the pinned digest (FR-17.17).
	if err := verifyContentDigest(entry.RootfsPath, entry.Digest); err != nil {
		return ResolvedImage{}, err
	}

	return ResolvedImage{
		KernelPath: entry.KernelPath,
		RootfsPath: entry.RootfsPath,
		Cmdline:    entry.Cmdline,
	}, nil
}

// readLock loads and parses images.lock fresh.
func (s *Store) readLock() (Lock, error) {
	data, err := os.ReadFile(filepath.Join(s.hostRoot, lockFileName)) //nolint:gosec // host-owned config path
	if err != nil {
		return Lock{}, fmt.Errorf("images: read %s: %w", lockFileName, err)
	}
	var lock Lock
	if err := json.Unmarshal(data, &lock); err != nil {
		return Lock{}, fmt.Errorf("images: parse %s: %w", lockFileName, err)
	}
	return lock, nil
}

// verifyContentDigest recomputes the rootfs SHA-256 and compares it to
// the pinned digest. The hash is STREAMED (io.Copy into the digest),
// never buffered: rootfs images are multi-GB and this runs at every
// boot. Refs: FR-17.17, NFR-17.1
func verifyContentDigest(rootfsPath, pinnedDigest string) error {
	file, err := os.Open(rootfsPath) //nolint:gosec // path from host-owned lock
	if err != nil {
		return fmt.Errorf("%w: read rootfs %s: %w", model.ErrVerificationFailed, rootfsPath, err)
	}
	defer func() { _ = file.Close() }()

	hasher := sha256.New()
	if _, err := io.Copy(hasher, file); err != nil {
		return fmt.Errorf("%w: hash rootfs %s: %w", model.ErrVerificationFailed, rootfsPath, err)
	}
	got := "sha256:" + hex.EncodeToString(hasher.Sum(nil))
	if got != pinnedDigest {
		return fmt.Errorf("%w: rootfs %s hashes to %s, pinned %s",
			model.ErrVerificationFailed, rootfsPath, got, pinnedDigest)
	}
	return nil
}
