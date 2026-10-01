package basecache

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// pinnersDir holds the back-references: pinners/<entry hex>/<root key>.json,
// one file per repository that pinned the entry. It is a sibling of the
// entries, never inside one, because an entry's bytes are its name.
// Refs: MGIT-239
const pinnersDir = "pinners"

// pinnerRecord is what one back-reference says: the host config root whose
// images.lock pinned the entry.
type pinnerRecord struct {
	HostRoot string `json:"host_root"`
}

// Listed is one published entry found in the cache.
type Listed struct {
	Digest string // "sha256:<hex>"
	Path   string // where its bytes live
}

// RecordPinner records that the repository whose sandbox config root is
// hostRoot pins digest.
//
// WHY A RECORD AND NOT A SCAN. Which repositories pin an entry is a fact
// only the pinning moment knows. A scan for images.lock files is a guess
// bounded by where it looked: on 2026-09-24 a full-disk stop found 27 of 35
// entries pinned by nothing a scan could find. So each pin leaves a
// back-reference here, and prune re-reads each named root's CURRENT lock to
// learn whether the pin still holds.
//
// APPEND-SAFE. One file per (entry, root), named by a hash of the root's
// resolved path, written by a rename: recording twice is a no-op, two
// recorders never write the same bytes into one file, and a crash leaves
// either the whole record or none. Nothing is ever rewritten in place.
// Refs: MGIT-239
func (c *Cache) RecordPinner(digest, hostRoot string) error {
	hexPart, err := parseDigest(digest)
	if err != nil {
		return err
	}
	root := resolvedRoot(hostRoot)
	dir := filepath.Join(c.root, pinnersDir, hexPart)
	final := filepath.Join(dir, rootKey(root)+".json")
	if _, err := os.Stat(final); err == nil {
		return nil
	}
	if err := c.markRoot(); err != nil {
		return fmt.Errorf("base cache: record pinner: %w", err)
	}
	if err := os.MkdirAll(dir, 0o750); err != nil {
		return fmt.Errorf("base cache: record pinner: %w", err)
	}
	body, err := json.Marshal(pinnerRecord{HostRoot: root})
	if err != nil {
		return fmt.Errorf("base cache: encode pinner: %w", err)
	}
	tmp, err := os.CreateTemp(dir, ".pinner-")
	if err != nil {
		return fmt.Errorf("base cache: record pinner: %w", err)
	}
	_, werr := tmp.Write(append(body, '\n'))
	if cerr := tmp.Close(); werr == nil {
		werr = cerr
	}
	if werr == nil {
		werr = os.Rename(tmp.Name(), final)
	}
	if werr != nil {
		_ = os.Remove(tmp.Name()) // best effort: the record failed either way, and that is what is returned
		return fmt.Errorf("base cache: record pinner of %s: %w", digest, werr)
	}
	return nil
}

// Pinners returns the host roots recorded as pinning digest, in path order.
// recorded is false when the entry has no back-reference at all — an entry
// composed before records existed, whose pinners nobody can name.
// Refs: MGIT-239
func (c *Cache) Pinners(digest string) (roots []string, recorded bool, err error) {
	hexPart, err := parseDigest(digest)
	if err != nil {
		return nil, false, err
	}
	dir := filepath.Join(c.root, pinnersDir, hexPart)
	files, err := os.ReadDir(dir)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, fmt.Errorf("base cache: read pinners of %s: %w", digest, err)
	}
	for _, f := range files {
		if f.IsDir() || !strings.HasSuffix(f.Name(), ".json") {
			continue // a recorder's temporary file, mid-rename
		}
		data, err := os.ReadFile(filepath.Join(dir, f.Name())) //nolint:gosec // a file mgit wrote under its own cache root
		if err != nil {
			return nil, false, fmt.Errorf("base cache: read pinner of %s: %w", digest, err)
		}
		var rec pinnerRecord
		if err := json.Unmarshal(data, &rec); err != nil || rec.HostRoot == "" {
			return nil, false, fmt.Errorf("base cache: pinner record %s of %s is unreadable", f.Name(), digest)
		}
		roots = append(roots, rec.HostRoot)
	}
	sort.Strings(roots)
	return roots, len(roots) > 0, nil
}

// Entries lists every published entry, in digest order. Staging trees are not
// entries, and neither is anything under sha256/ that is not named by a digest.
func (c *Cache) Entries() ([]Listed, error) {
	dirents, err := os.ReadDir(filepath.Join(c.root, entriesDir))
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("base cache: list entries: %w", err)
	}
	var out []Listed
	for _, d := range dirents {
		digest := digestAlgo + ":" + d.Name()
		if !d.IsDir() {
			continue
		}
		if _, err := parseDigest(digest); err != nil {
			continue
		}
		out = append(out, Listed{Digest: digest, Path: filepath.Join(c.root, entriesDir, d.Name())})
	}
	return out, nil
}

// Remove deletes one published entry.
//
// The entry is first RENAMED out of sha256/ into staging, so a reader either
// finds the complete entry or no entry — never a half-deleted tree that still
// answers Has — and then removed. If the removal itself is interrupted, the
// remains are staging debris, which PruneStaging collects. Refs: MGIT-239
func (c *Cache) Remove(digest string) error {
	path, err := c.Path(digest)
	if err != nil {
		return err
	}
	if !c.Has(digest) {
		return fmt.Errorf("base cache: %s is not in the cache", digest)
	}
	parent := filepath.Join(c.root, stagingDir)
	if err := os.MkdirAll(parent, 0o750); err != nil {
		return fmt.Errorf("base cache: remove %s: %w", digest, err)
	}
	doomed, err := os.MkdirTemp(parent, "prune-")
	if err != nil {
		return fmt.Errorf("base cache: remove %s: %w", digest, err)
	}
	target := filepath.Join(doomed, "entry")
	if err := os.Rename(path, target); err != nil {
		_ = os.Remove(doomed) // best effort: empty, and the rename failure is what matters
		return fmt.Errorf("base cache: remove %s: %w", digest, err)
	}
	if err := os.RemoveAll(doomed); err != nil {
		return fmt.Errorf("base cache: remove %s (moved to %s): %w", digest, doomed, err)
	}
	return nil
}

// TreeBytes sums the sizes of the regular files under dir. Symlinks are
// neither followed nor counted: a base's links point inside the guest.
func TreeBytes(dir string) (int64, error) {
	var total int64
	err := filepath.WalkDir(dir, func(_ string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if !d.Type().IsRegular() {
			return nil
		}
		info, err := d.Info()
		if err != nil {
			return err
		}
		total += info.Size()
		return nil
	})
	if err != nil {
		return 0, fmt.Errorf("base cache: size %s: %w", dir, err)
	}
	return total, nil
}

// resolvedRoot is the path a back-reference records: absolute, symlinks
// resolved, so two spellings of one repository record once.
func resolvedRoot(p string) string {
	if abs, err := filepath.Abs(p); err == nil {
		p = abs
	}
	if resolved, err := filepath.EvalSymlinks(p); err == nil {
		return resolved
	}
	return filepath.Clean(p)
}

// rootKey names a root's back-reference file: a fixed-length hash, so any
// path, however long or oddly spelled, becomes one safe file name.
func rootKey(root string) string {
	sum := sha256.Sum256([]byte(root))
	return hex.EncodeToString(sum[:16])
}
