package images

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
)

// TreeDigest computes a stable content digest over a whole DIRECTORY, in the
// same `sha256:<hex>` form ComputeDigest produces for a file.
//
// It exists because a libkrun guest base is a directory, not a rootfs image:
// libkrunfw supplies the kernel and the guest root is shared over virtio-fs.
// Pinning it must therefore give trees the property files already have — a
// base cannot be swapped silently under a task that pinned it (MGIT-61.15).
//
// WHAT IS HASHED, and why each part is load-bearing:
//   - the RELATIVE PATH of every entry, so a rename is a different tree even
//     though the bytes are unchanged;
//   - the ENTRY KIND (file, directory, symlink), so replacing a file with a
//     directory of the same name is visible;
//   - the EXECUTABLE BIT, so a data file that becomes executable — or a
//     supervisor that stops being — is a different base. Only that bit: full
//     permission bits vary with the extractor's umask and would make honest
//     re-materialization look like tampering;
//   - file CONTENT, and for a symlink its TARGET.
//
// What is deliberately NOT hashed: mtimes, uid/gid, and the rest of the mode.
// A base re-extracted from the same source must pin identically, or the check
// cries wolf and people learn to ignore it.
//
// Entries are walked in lexical order (filepath.WalkDir guarantees it), so
// the digest is deterministic without an explicit sort.
// Refs: FR-17.17, FR-17.29, MGIT-61.15, ADR-002
func TreeDigest(root string) (string, error) {
	info, err := os.Stat(root)
	if err != nil {
		return "", fmt.Errorf("images: stat tree %s: %w", root, err)
	}
	if !info.IsDir() {
		return "", fmt.Errorf("images: %s is not a directory; use ComputeDigest for a file", root)
	}
	abs, err := filepath.Abs(root)
	if err != nil {
		return "", fmt.Errorf("images: resolve tree %s: %w", root, err)
	}

	hasher := sha256.New()
	err = filepath.WalkDir(abs, func(path string, d os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		rel, err := filepath.Rel(abs, path)
		if err != nil {
			return err
		}
		if rel == "." {
			return nil
		}
		return hashEntry(hasher, abs, path, rel, d)
	})
	if err != nil {
		return "", err
	}
	return "sha256:" + hex.EncodeToString(hasher.Sum(nil)), nil
}

// hashEntry folds one tree entry into the running hash. Each record is
// length-delimited by the newline-terminated header, so no combination of
// names and contents can be made to collide with a different tree by
// concatenation.
func hashEntry(hasher io.Writer, root, path, rel string, d os.DirEntry) error {
	switch {
	case d.IsDir():
		_, err := fmt.Fprintf(hasher, "dir %s\n", rel)
		return err

	case d.Type()&os.ModeSymlink != 0:
		// Symlinks inside the tree are normal in a userspace base (bin/sh ->
		// busybox), so they are pinned by TARGET rather than followed.
		target, err := os.Readlink(path)
		if err != nil {
			return fmt.Errorf("images: read symlink %s: %w", rel, err)
		}
		if err := assertSymlinkWithinTree(root, path, rel); err != nil {
			return err
		}
		_, err = fmt.Fprintf(hasher, "link %s -> %s\n", rel, target)
		return err

	case d.Type().IsRegular():
		info, err := d.Info()
		if err != nil {
			return err
		}
		// Only the executable bit: the rest of the mode varies with the
		// extractor's umask and is not part of the base's identity.
		exec := 0
		if info.Mode()&0o111 != 0 {
			exec = 1
		}
		if _, err := fmt.Fprintf(hasher, "file %s %d %d\n", rel, exec, info.Size()); err != nil {
			return err
		}
		file, err := os.Open(path) //nolint:gosec // path came from walking the caller's own tree
		if err != nil {
			return fmt.Errorf("images: open %s: %w", rel, err)
		}
		defer func() { _ = file.Close() }()
		if _, err := io.Copy(hasher, file); err != nil {
			return fmt.Errorf("images: hash %s: %w", rel, err)
		}
		return nil

	default:
		// Sockets, devices and fifos have no content to pin, and a base that
		// needs them is not something we can honestly claim to have verified.
		return fmt.Errorf("images: tree entry %s is a %s, which cannot be content-pinned",
			rel, d.Type().String())
	}
}

// assertSymlinkWithinTree rejects a symlink whose target resolves outside the
// tree. Such a link would make the digest a claim about files the base does
// not contain — pinning a moving target the host can change afterwards.
//
// An ABSOLUTE target is guest-relative, not a host path: once the guest has
// pivoted into this tree, /usr/bin/mawk names a file in the base. Every real
// distro image relies on that (debian:12 ships dozens under
// /etc/alternatives), so treating absolute targets as escapes would refuse
// almost every usable base. If such a target names something the base lacks,
// the guest gets ENOENT — a dangling link, not a containment failure.
//
// What genuinely escapes is a RELATIVE target that climbs out with .., because
// that resolves the same way on both sides of the boundary — including on the
// host, which is the side that must not be reachable.
// Refs: SEC-03, FR-17.17, MGIT-61.15
func assertSymlinkWithinTree(root, path, rel string) error {
	target, err := os.Readlink(path)
	if err != nil {
		return fmt.Errorf("images: read symlink %s: %w", rel, err)
	}
	if filepath.IsAbs(target) {
		return nil
	}
	resolved := filepath.Clean(filepath.Join(filepath.Dir(path), target))
	if resolved != root && !strings.HasPrefix(resolved, root+string(os.PathSeparator)) {
		return fmt.Errorf(
			"images: symlink %s -> %s escapes the guest base tree; a base cannot be "+
				"pinned when part of it lives outside", rel, target)
	}
	return nil
}
