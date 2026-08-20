// Package basecache is the machine-wide, content-addressed store for composed
// guest bases.
//
// WHY IT EXISTS. A guest base used to be unpacked INTO the repository at
// .mgit/sandbox/base — 175 MB and 5,240 files here, 906 MB in a pilot's
// repo. Two things went wrong with that, and the second is the worse one:
//
//   - it is inside the tree, so every host test command that walks the
//     repository sees it. A pilot's `gofmt -l .` red-lit on 757 files that
//     were mgit's artifact and not theirs — a containment tool breaking the
//     host repository's own test command, at the first thing a newly
//     sandboxed lane runs;
//   - it is MUTABLE. `mgit sandbox base from` rewrote that one directory in
//     place, so recomposing in one worktree silently invalidated the pinned
//     digest of every other worktree sharing the repo. Verification failed
//     closed (which is why nothing unsafe happened), but the drift should be
//     impossible, not merely detected.
//
// THE FIX, and its one non-negotiable property: one base per DIGEST, out of
// tree, immutable by construction. A shared-but-MUTABLE cache would be worse
// than the in-tree one — it would give a single repo the power to invalidate
// every repo on the machine. So the digest is the key AND the path: a
// recompose whose bytes differ hashes differently and therefore lands beside
// the old entry rather than on top of it. Nothing in mgit ever writes into a
// published entry; publishing is a rename of a privately staged tree.
//
// This is the identical-bytes law (MGIT-105/106) applied to our own base
// image, and the same shape as MGIT-143's downloads cache.
//
// Refs: MGIT-147, MGIT-105, MGIT-143, FR-17.17, ADR-010
package basecache

import (
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// EnvRoot overrides the cache location. It exists for tests and for hosts
// that keep large caches on a specific volume; a real user never sets it.
const EnvRoot = "MGIT_BASE_CACHE"

// Layout under the cache root. Staging is a SIBLING of the published tree,
// deliberately: publishing is an os.Rename, and a rename is only atomic
// within one filesystem.
const (
	entriesDir = "sha256"
	stagingDir = "staging"
)

// digestAlgo is the only algorithm a cache key may name. mgit's own integrity
// hash is SHA-256 (ADR-002); a second algorithm would mean two names for one
// byte sequence, which is exactly what content addressing exists to prevent.
const digestAlgo = "sha256"

// TreeDigester hashes a directory tree into a "sha256:<hex>" digest.
//
// It is injected rather than imported so this package does not depend on the
// image store — the image store depends on THIS one, to turn a pinned digest
// into a path. Production passes images.TreeDigest. Refs: MGIT-147
type TreeDigester func(dir string) (string, error)

// Entry is one published, immutable base in the cache.
type Entry struct {
	Digest string // "sha256:<hex>" of the tree — its identity AND its name
	Path   string // where the bytes live; derived from Digest, never stored
	// Deduplicated reports that these bytes were already cached and this
	// call published nothing. It is the normal outcome of the second and
	// every later population of the same digest, including a race.
	Deduplicated bool
}

// Cache is a content-addressed base store rooted at one directory.
type Cache struct {
	root string
}

// New opens a cache at an explicit root. The directory is created lazily, on
// the first population, so merely constructing a Cache touches no disk.
func New(root string) (*Cache, error) {
	if root == "" {
		return nil, fmt.Errorf("base cache: root must not be empty")
	}
	return &Cache{root: root}, nil
}

// Open opens the cache at DefaultRoot.
func Open() (*Cache, error) {
	root, err := DefaultRoot()
	if err != nil {
		return nil, err
	}
	return New(root)
}

// DefaultRoot is the machine-wide cache location for this user.
//
// os.UserCacheDir is the whole convention story: on Linux it reads
// XDG_CACHE_HOME and falls back to ~/.cache, and on macOS it is
// ~/Library/Caches. Both are per-USER rather than per-repo, which is the
// population this has to serve — several repos and several agents on one
// machine, each of which would otherwise pay its own copy of a ~900 MB tree.
// Refs: MGIT-147
func DefaultRoot() (string, error) {
	if override := strings.TrimSpace(os.Getenv(EnvRoot)); override != "" {
		if !filepath.IsAbs(override) {
			return "", fmt.Errorf(
				"base cache: %s must be an absolute path, got %q — a relative "+
					"cache root would name a different directory for every "+
					"process depending on where it was started", EnvRoot, override)
		}
		return filepath.Clean(override), nil
	}
	dir, err := os.UserCacheDir()
	if err != nil {
		return "", fmt.Errorf("base cache: locate the OS cache directory (set %s to override): %w", EnvRoot, err)
	}
	return filepath.Join(dir, "mgit", "bases"), nil
}

// Root is the directory this cache occupies.
func (c *Cache) Root() string { return c.root }

// Path is where the entry for a digest lives — whether or not it is populated.
//
// The mapping is total and pure: no lookup, no index, no lock file. That is
// what "identity is the path" buys, and it is why verification gets CHEAPER
// rather than unnecessary — the digest check still runs, but finding the
// bytes no longer needs a stored path that something could repoint.
// Refs: MGIT-147, FR-17.17
func (c *Cache) Path(digest string) (string, error) {
	hexPart, err := parseDigest(digest)
	if err != nil {
		return "", err
	}
	return filepath.Join(c.root, entriesDir, hexPart), nil
}

// Has reports whether a digest is populated. A malformed digest is simply
// absent: it can never name an entry.
func (c *Cache) Has(digest string) bool {
	path, err := c.Path(digest)
	if err != nil {
		return false
	}
	info, err := os.Stat(path)
	return err == nil && info.IsDir()
}

// Stage creates a private directory to compose a base into. It lives inside
// the cache root so publishing it is a same-filesystem rename.
//
// The caller owns the returned tree until it Commits (which consumes it) or
// Discards it. A crashed compose leaves it behind; PruneStaging collects it.
func (c *Cache) Stage() (string, error) {
	parent := filepath.Join(c.root, stagingDir)
	if err := os.MkdirAll(parent, 0o750); err != nil {
		return "", fmt.Errorf("base cache: create staging area: %w", err)
	}
	dir, err := os.MkdirTemp(parent, "compose-")
	if err != nil {
		return "", fmt.Errorf("base cache: create staging tree: %w", err)
	}
	return dir, nil
}

// Discard removes a staging tree the caller is abandoning.
func (c *Cache) Discard(staging string) error {
	if err := os.RemoveAll(staging); err != nil {
		return fmt.Errorf("base cache: discard staging tree %s: %w", staging, err)
	}
	return nil
}

// Commit hashes a staged tree and publishes it under its digest, consuming
// the staging tree either way.
//
// CONCURRENCY, which is the whole reason this is one function and not three.
// Two processes composing the same base each hash their own private tree and
// then rename it to the same destination. A rename onto a populated entry
// fails (the destination is a non-empty directory), and that failure is the
// SUCCESS case for the loser: the bytes are already there, under a name that
// can only mean these bytes, so it drops its copy and returns the winner's
// entry. No lock file, no partially visible tree — a reader either sees the
// complete entry or does not see the directory at all.
// Refs: MGIT-147, MGIT-105
func (c *Cache) Commit(staging string, digestTree TreeDigester) (Entry, error) {
	if digestTree == nil {
		return Entry{}, fmt.Errorf("base cache: tree digester must not be nil")
	}
	digest, err := digestTree(staging)
	if err != nil {
		return Entry{}, fmt.Errorf("base cache: digest staged tree: %w", err)
	}
	final, err := c.Path(digest)
	if err != nil {
		return Entry{}, err
	}
	if err := os.MkdirAll(filepath.Dir(final), 0o750); err != nil {
		return Entry{}, fmt.Errorf("base cache: create entries directory: %w", err)
	}
	entry := Entry{Digest: digest, Path: final}
	if c.Has(digest) {
		return entry.dedupe(c.Discard(staging))
	}
	if err := os.Rename(staging, final); err != nil {
		// Lost the race, or the entry appeared between the check and here.
		if c.Has(digest) {
			return entry.dedupe(c.Discard(staging))
		}
		return Entry{}, fmt.Errorf("base cache: publish %s: %w", digest, err)
	}
	return entry, nil
}

// dedupe marks an entry as already-present, folding in the error from
// dropping the caller's redundant copy.
func (e Entry) dedupe(err error) (Entry, error) {
	if err != nil {
		return Entry{}, err
	}
	e.Deduplicated = true
	return e, nil
}

// Adopt takes an EXISTING tree — an in-tree base left by an older mgit — into
// the cache under the digest it already hashes to.
//
// The digest is unchanged by the move, which is the point: every images.lock
// that pinned the in-tree base keeps resolving, so migration costs nobody
// their pin. A move is attempted first (instant on one filesystem); a copy is
// the fallback when the repo and the cache are on different volumes.
// Refs: MGIT-147
func (c *Cache) Adopt(dir string, digestTree TreeDigester) (Entry, error) {
	if digestTree == nil {
		return Entry{}, fmt.Errorf("base cache: tree digester must not be nil")
	}
	digest, err := digestTree(dir)
	if err != nil {
		return Entry{}, fmt.Errorf("base cache: digest %s: %w", dir, err)
	}
	final, err := c.Path(digest)
	if err != nil {
		return Entry{}, err
	}
	if err := os.MkdirAll(filepath.Dir(final), 0o750); err != nil {
		return Entry{}, fmt.Errorf("base cache: create entries directory: %w", err)
	}
	entry := Entry{Digest: digest, Path: final}
	if c.Has(digest) {
		// The identical bytes are already cached, so the in-tree copy is
		// redundant by definition — dropping it is the migration.
		return entry.dedupe(c.Discard(dir))
	}
	if err := os.Rename(dir, final); err == nil {
		return entry, nil
	}
	return c.adoptByCopy(dir, digest, entry)
}

// adoptByCopy is the cross-filesystem fallback: copy into staging, publish
// through the ordinary Commit path (so the race semantics are identical),
// then drop the original.
func (c *Cache) adoptByCopy(dir, digest string, entry Entry) (Entry, error) {
	if c.Has(digest) {
		return entry.dedupe(c.Discard(dir))
	}
	staging, err := c.Stage()
	if err != nil {
		return Entry{}, err
	}
	if err := copyTree(dir, staging); err != nil {
		_ = c.Discard(staging)
		return Entry{}, fmt.Errorf("base cache: adopt %s: %w", dir, err)
	}
	published, err := c.Commit(staging, func(string) (string, error) { return digest, nil })
	if err != nil {
		return Entry{}, err
	}
	if err := c.Discard(dir); err != nil {
		return Entry{}, err
	}
	published.Deduplicated = false
	return published, nil
}

// PruneStaging removes staging trees older than maxAge — the debris a
// compose that crashed mid-pull leaves behind. Each is a full base, so
// leaking them is a multi-GB problem, and none of them is ever a published
// entry: an entry is only ever created by a rename OUT of staging.
func (c *Cache) PruneStaging(now time.Time, maxAge time.Duration) (int, error) {
	parent := filepath.Join(c.root, stagingDir)
	entries, err := os.ReadDir(parent)
	if err != nil {
		if os.IsNotExist(err) {
			return 0, nil
		}
		return 0, fmt.Errorf("base cache: read staging area: %w", err)
	}
	removed := 0
	for _, e := range entries {
		info, err := e.Info()
		if err != nil || now.Sub(info.ModTime()) < maxAge {
			continue
		}
		if err := os.RemoveAll(filepath.Join(parent, e.Name())); err != nil {
			return removed, fmt.Errorf("base cache: prune staging tree %s: %w", e.Name(), err)
		}
		removed++
	}
	return removed, nil
}

// parseDigest validates a "sha256:<64 hex>" digest and returns the hex half,
// which is the only part that becomes a path component.
//
// Validation is not decoration: the digest arrives from images.lock, and a
// digest containing path separators would otherwise let a lock entry name a
// directory outside the cache.
func parseDigest(digest string) (string, error) {
	algo, hexPart, ok := strings.Cut(digest, ":")
	if !ok || algo != digestAlgo {
		return "", fmt.Errorf("base cache: digest %q must be %s:<hex>", digest, digestAlgo)
	}
	if len(hexPart) != 64 {
		return "", fmt.Errorf("base cache: digest %q must have 64 hex characters, got %d", digest, len(hexPart))
	}
	if _, err := hex.DecodeString(hexPart); err != nil {
		return "", fmt.Errorf("base cache: digest %q is not hexadecimal: %w", digest, err)
	}
	return hexPart, nil
}
