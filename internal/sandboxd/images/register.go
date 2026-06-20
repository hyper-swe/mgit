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

	"github.com/hyper-swe/mgit/internal/sandboxd/hostkey"
)

// ComputeDigest returns the "sha256:<hex>" content digest of a file. The
// hash is STREAMED (io.Copy into the digest), never buffered, so multi-GB
// guest images are handled. It is the host-side counterpart to the
// boot-time content verification (verifyContentDigest). Refs: FR-17.17
func ComputeDigest(path string) (string, error) {
	file, err := os.Open(path) //nolint:gosec // host-supplied image path
	if err != nil {
		return "", fmt.Errorf("images: open %s: %w", path, err)
	}
	defer func() { _ = file.Close() }()
	hasher := sha256.New()
	if _, err := io.Copy(hasher, file); err != nil {
		return "", fmt.Errorf("images: hash %s: %w", path, err)
	}
	return "sha256:" + hex.EncodeToString(hasher.Sum(nil)), nil
}

// BuildEntry computes the kernel and rootfs content digests and returns
// an UNSIGNED image entry. The caller signs it (Sign) or registers it
// (Register) before it can be resolved. Refs: FR-17.17, FR-17.29
func BuildEntry(kernelPath, rootfsPath, cmdline string) (Entry, error) {
	rootfsDigest, err := ComputeDigest(rootfsPath)
	if err != nil {
		return Entry{}, err
	}
	kernelDigest, err := ComputeDigest(kernelPath)
	if err != nil {
		return Entry{}, err
	}
	return Entry{
		Digest:       rootfsDigest,
		KernelDigest: kernelDigest,
		KernelPath:   kernelPath,
		RootfsPath:   rootfsPath,
		Cmdline:      cmdline,
	}, nil
}

// Sign returns a copy of e with its detached Ed25519 Signature set for
// the given image name, over the canonical SigningPayload (name + both
// digests + cmdline). The host signer and the boot-time verifier
// (Store.Resolve) produce identical payload bytes. Refs: FR-17.29
func Sign(name string, e Entry, priv ed25519.PrivateKey) Entry {
	e.Signature = ed25519.Sign(priv, SigningPayload(name, e))
	return e
}

// LoadSigningKey reads the trust-root private signing key from
// hostRoot/trust (written by GenerateTrustRoot). It fails closed on a
// missing or malformed key. Keep the returned key host-side; it never
// enters a guest or image (SEC-01). Refs: FR-17.38
func LoadSigningKey(hostRoot string) (ed25519.PrivateKey, error) {
	path := filepath.Join(hostRoot, hostkey.TrustDirName, signingKey)
	data, err := os.ReadFile(path) //nolint:gosec // host-owned trust-root path
	if err != nil {
		return nil, fmt.Errorf("images: read signing key (run GenerateTrustRoot first): %w", err)
	}
	if len(data) != ed25519.PrivateKeySize {
		return nil, fmt.Errorf("images: signing key is %d bytes, want %d", len(data), ed25519.PrivateKeySize)
	}
	return ed25519.PrivateKey(data), nil
}

// Register signs the entry for name and atomically writes it into
// images.lock under hostRoot, preserving any other entries. It returns
// the digest-pinned image reference (<name>@<rootfs-digest>) callers pass
// to the sandbox. A registered image is exactly what Store.Resolve
// verifies and admits. Refs: FR-17.17, FR-17.29, FR-17.36
func Register(hostRoot, name string, e Entry, priv ed25519.PrivateKey) (string, error) {
	if hostRoot == "" {
		return "", fmt.Errorf("images register: host root must not be empty")
	}
	if name == "" {
		return "", fmt.Errorf("images register: image name must not be empty")
	}
	lock, err := readLockFile(hostRoot)
	if err != nil {
		return "", err
	}
	if lock.Images == nil {
		lock.Images = make(map[string]Entry)
	}
	lock.Images[name] = Sign(name, e, priv)
	if err := writeLockFile(hostRoot, lock); err != nil {
		return "", err
	}
	return name + "@" + e.Digest, nil
}

// readLockFile loads images.lock, returning an empty lock if it does not
// yet exist (the first Register creates it).
func readLockFile(hostRoot string) (Lock, error) {
	data, err := os.ReadFile(filepath.Join(hostRoot, lockFileName)) //nolint:gosec // host-owned config path
	if err != nil {
		if os.IsNotExist(err) {
			return Lock{}, nil
		}
		return Lock{}, fmt.Errorf("images: read %s: %w", lockFileName, err)
	}
	var lock Lock
	if err := json.Unmarshal(data, &lock); err != nil {
		return Lock{}, fmt.Errorf("images: parse %s: %w", lockFileName, err)
	}
	return lock, nil
}

// writeLockFile atomically writes images.lock (owner-only) under hostRoot.
func writeLockFile(hostRoot string, lock Lock) error {
	data, err := json.MarshalIndent(lock, "", "  ")
	if err != nil {
		return fmt.Errorf("images: encode %s: %w", lockFileName, err)
	}
	if err := hostkey.WriteFileAtomic(filepath.Join(hostRoot, lockFileName), data); err != nil {
		return fmt.Errorf("images: write %s: %w", lockFileName, err)
	}
	return nil
}
