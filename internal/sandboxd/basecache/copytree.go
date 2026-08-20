package basecache

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
)

// copyTree recursively copies src into dst, preserving what the tree digest
// hashes: relative paths, entry kinds, symlink targets, and the file mode.
//
// It is the cross-filesystem fallback for Adopt only. The ordinary path never
// copies a base — a compose stages inside the cache root, so publishing is a
// rename. Refs: MGIT-147
func copyTree(src, dst string) error {
	return filepath.WalkDir(src, func(path string, d os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		rel, err := filepath.Rel(src, path)
		if err != nil {
			return err
		}
		out := filepath.Join(dst, rel)
		switch {
		case d.IsDir():
			return copyDir(path, out)
		case d.Type()&os.ModeSymlink != 0:
			target, err := os.Readlink(path)
			if err != nil {
				return fmt.Errorf("read symlink %s: %w", rel, err)
			}
			// Copied verbatim, dangling or not: a guest base is full of
			// symlinks that only resolve once it is the guest's root, and
			// rewriting one would change the tree's digest.
			//nolint:gosec // G122: the source is a host-composed base tree, not guest-controlled input
			return os.Symlink(target, out)
		default:
			return copyFile(path, out)
		}
	})
}

// copyDir recreates a directory with the source's permissions.
func copyDir(src, dst string) error {
	info, err := os.Stat(src)
	if err != nil {
		return fmt.Errorf("stat %s: %w", src, err)
	}
	if err := os.MkdirAll(dst, info.Mode().Perm()); err != nil {
		return fmt.Errorf("mkdir %s: %w", dst, err)
	}
	// MkdirAll masks the mode with the process umask, so a base directory
	// that must be traversable inside the guest is set explicitly.
	if err := os.Chmod(dst, info.Mode().Perm()); err != nil {
		return fmt.Errorf("chmod %s: %w", dst, err)
	}
	return nil
}

// copyFile copies one regular file, applying the source mode with an explicit
// chmod. O_CREATE's mode is masked by the process umask, which would silently
// strip the executable bit off a guest binary — and the executable bit is part
// of what the tree digest covers. Refs: MGIT-81, MGIT-147
func copyFile(src, dst string) error {
	info, err := os.Lstat(src)
	if err != nil {
		return fmt.Errorf("stat %s: %w", src, err)
	}
	in, err := os.Open(src) //nolint:gosec // path from a host-owned base tree walk
	if err != nil {
		return fmt.Errorf("open %s: %w", src, err)
	}
	defer func() { _ = in.Close() }()
	out, err := os.OpenFile(dst, os.O_CREATE|os.O_EXCL|os.O_WRONLY, info.Mode().Perm()) //nolint:gosec // cache-owned staging tree
	if err != nil {
		return fmt.Errorf("create %s: %w", dst, err)
	}
	if _, err := io.Copy(out, in); err != nil {
		_ = out.Close()
		return fmt.Errorf("copy %s: %w", dst, err)
	}
	if err := out.Close(); err != nil {
		return fmt.Errorf("close %s: %w", dst, err)
	}
	if err := os.Chmod(dst, info.Mode().Perm()); err != nil {
		return fmt.Errorf("chmod %s: %w", dst, err)
	}
	return nil
}
