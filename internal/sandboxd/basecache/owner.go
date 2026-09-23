package basecache

import (
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// RootMarker is the file every cache root carries, so a directory can say it
// is one whatever path is used to reach it. Refs: MGIT-226
const RootMarker = ".mgit-base-cache"

const rootMarkerBody = "This directory is mgit's guest-base cache. Every entry under sha256/ is\n" +
	"named by its content and pinned by the repositories that composed it:\n" +
	"nothing may write into one. To use a cached userspace, copy it out first.\n"

// markRoot records that c.root is a base cache. It is idempotent, and it runs
// wherever the cache stages or publishes, so every root mgit writes into
// carries the marker. Its errors are bare: each caller says what it was doing.
func (c *Cache) markRoot() error {
	if err := os.MkdirAll(c.root, 0o750); err != nil {
		return fmt.Errorf("create root: %w", err)
	}
	marker := filepath.Join(c.root, RootMarker)
	if _, err := os.Stat(marker); err == nil {
		return nil
	}
	if err := os.WriteFile(marker, []byte(rootMarkerBody), 0o600); err != nil {
		return fmt.Errorf("mark root: %w", err)
	}
	return nil
}

// SystemRoot is the machine-wide cache location with NO override applied:
// where every repository's cache lives unless its process says otherwise. A
// process pointed elsewhere must still recognize it. Refs: MGIT-226
func SystemRoot() (string, error) {
	dir, err := os.UserCacheDir()
	if err != nil {
		return "", fmt.Errorf("base cache: locate the OS cache directory: %w", err)
	}
	return filepath.Join(dir, "mgit", "bases"), nil
}

// Owner returns the base cache root that dir lies inside, or "" when it lies
// inside none.
//
// IDENTITY, NOT SPELLING. A string comparison against a root is defeated by
// every other name for the same directory: a case variant on a case-folding
// filesystem, macOS's data-volume firmlink, or a root this process was never
// told about. So Owner resolves dir through symlinks and walks its ancestors.
// At each one it asks the directory itself whether it carries RootMarker, and
// the filesystem whether it IS one of knownRoots (os.SameFile). knownRoots
// covers caches written before the marker existed. A known root that does
// not exist holds no entries and is skipped. An ENTRY is recognized by its
// layout as well, sha256/<64 hex>, which is how an older cache at a root
// nobody names still says what it is. Refs: MGIT-226, MGIT-147
func Owner(dir string, knownRoots []string) (string, error) {
	start, err := filepath.EvalSymlinks(dir)
	if err != nil {
		if start, err = filepath.Abs(dir); err != nil {
			return "", fmt.Errorf("base cache: resolve %s: %w", dir, err)
		}
	}
	var roots []os.FileInfo
	for _, r := range knownRoots {
		if info, statErr := os.Stat(r); statErr == nil {
			roots = append(roots, info)
		}
	}
	for a := start; ; {
		if _, statErr := os.Stat(filepath.Join(a, RootMarker)); statErr == nil {
			return a, nil
		}
		if isEntryDir(a) {
			return filepath.Dir(filepath.Dir(a)), nil
		}
		if info, statErr := os.Stat(a); statErr == nil {
			for _, r := range roots {
				if os.SameFile(info, r) {
					return a, nil
				}
			}
		}
		parent := filepath.Dir(a)
		if parent == a {
			return "", nil
		}
		a = parent
	}
}

// isEntryDir reports whether dir has the shape of a published entry: a 64-hex
// name directly under a directory named sha256. Case is folded, as the
// filesystems it guards may fold it.
func isEntryDir(dir string) bool {
	name := filepath.Base(dir)
	if len(name) != 64 || !strings.EqualFold(filepath.Base(filepath.Dir(dir)), entriesDir) {
		return false
	}
	_, err := hex.DecodeString(name)
	return err == nil
}
