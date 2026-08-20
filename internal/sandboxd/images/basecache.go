package images

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// BaseLocator turns a pinned digest into the directory holding those bytes.
//
// It is the ONE thing the image store needs from the machine-wide base cache,
// and it is an interface so the store depends on a mapping rather than on a
// cache implementation — a test injects its own, and the daemon injects the
// real one. Refs: MGIT-147
type BaseLocator interface {
	// Path is where the entry for a digest lives, populated or not.
	Path(digest string) (string, error)
	// Has reports whether those bytes are actually present.
	Has(digest string) bool
	// Root is the cache directory, named in errors so a user can see which
	// store was searched.
	Root() string
}

// ErrBaseNotCached reports that a pinned guest base is not present in this
// machine's base cache.
//
// It is distinct from a verification failure on purpose: nothing is wrong with
// the pin, the bytes are simply not here — a fresh machine, a cleaned cache,
// or a repo cloned from somewhere else. The remedy is a re-compose, which is a
// user action rather than something mgit does silently. Refs: MGIT-147
var ErrBaseNotCached = errors.New("images: pinned guest base is not in the base cache")

// BuildCachedBaseEntry returns an UNSIGNED lock entry for a guest base that
// lives in the machine-wide cache: the pinned digest and NOTHING ELSE.
//
// No path is stored, and that is the point. The digest already determines
// where the bytes live, so a stored path could only ever be a second, weaker
// name for the same thing — one that a lock-writer could repoint and that
// pins the base to one machine's layout. The caller signs and registers it
// exactly as for any other entry; the signing payload is unchanged, because
// paths were never part of it. Refs: MGIT-147, FR-17.17, FR-17.29
func BuildCachedBaseEntry(digest string) Entry {
	return Entry{Digest: digest}
}

// LookupEntry returns the lock entry registered under name, or ErrNoSuchImage.
// It exists so a caller can compare what is pinned NOW against what it is
// about to pin — the provenance check for a tag that moved. Refs: MGIT-147
func LookupEntry(hostRoot, name string) (Entry, error) {
	lock, err := readLockFile(hostRoot)
	if err != nil {
		return Entry{}, err
	}
	entry, ok := lock.Images[name]
	if !ok {
		return Entry{}, fmt.Errorf("%w: %q", ErrNoSuchImage, name)
	}
	return entry, nil
}

// RepointToCache rewrites every lock entry that pointed INTO legacyPath so it
// is located by its digest instead, and returns the names it repointed.
//
// It is the lock half of migrating an in-tree base out of the repository. No
// re-signing happens and none is needed: the signing payload covers the name,
// both digests, the cmdline and the source — never a path — precisely so that
// where the bytes live can change without weakening what is verified about
// them. The digest is untouched, so a pin that resolved before resolves after.
// Refs: MGIT-147, FR-17.29
func RepointToCache(hostRoot, legacyPath string) ([]string, error) {
	lock, err := readLockFile(hostRoot)
	if err != nil {
		return nil, err
	}
	legacy := filepath.Clean(legacyPath)
	var repointed []string
	for name, entry := range lock.Images {
		if entry.RootfsPath == "" || filepath.Clean(entry.RootfsPath) != legacy {
			continue
		}
		entry.RootfsPath = ""
		lock.Images[name] = entry
		repointed = append(repointed, name)
	}
	if len(repointed) == 0 {
		return nil, nil
	}
	if err := writeLockFile(hostRoot, lock); err != nil {
		return nil, err
	}
	return repointed, nil
}

// locateBase resolves where an entry's rootfs bytes are.
//
// A non-empty RootfsPath is a FILE image (firecracker/vzf) or a
// bring-your-own base directory, and is used as given. An empty one names a
// cached guest base, whose location is derived from the digest — identity is
// the path. Refs: MGIT-147, FR-17.17
func (s *Store) locateBase(name string, entry Entry) (string, error) {
	if entry.RootfsPath != "" {
		return entry.RootfsPath, nil
	}
	if s.baseCache == nil {
		return "", fmt.Errorf("%w: %q is pinned to %s, but this process has no base cache "+
			"to look in (%s)", ErrBaseNotCached, name, entry.Digest, s.baseCacheErr)
	}
	path, err := s.baseCache.Path(entry.Digest)
	if err != nil {
		return "", fmt.Errorf("%w: %w", ErrBaseNotCached, err)
	}
	if !s.baseCache.Has(entry.Digest) {
		return "", notCachedError(name, entry, s.baseCache.Root())
	}
	return path, nil
}

// notCachedError explains a missing cache entry in the terms a user can act
// on: what was pinned, where we looked, what it was composed from, and the
// one command that puts it back.
//
// WHY MGIT DOES NOT RE-FETCH IT AUTOMATICALLY. A re-compose is not guaranteed
// to reproduce the pinned digest, for two independent reasons: a TAG can move
// under you (golang:1.26-bookworm resolved to two different images a day
// apart), and the composed tree includes the guest binaries THIS mgit build
// injects. So an automatic re-fetch would pull hundreds of megabytes to arrive
// at either the same error or — far worse — a temptation to accept different
// bytes under the old pin. Re-composing is therefore an explicit, announced
// user action, and this message is how the user learns to take it.
// Refs: MGIT-147
func notCachedError(name string, entry Entry, cacheRoot string) error {
	recompose := "  mgit sandbox base from <image>"
	if source := sourceTag(entry.Source); source != "" {
		recompose = "  mgit sandbox base from " + source
	}
	provenance := "composed locally (no OCI source recorded)"
	if entry.Source != "" {
		provenance = "composed from " + entry.Source
	}
	return fmt.Errorf("%w: %q is pinned to %s, %s, but those bytes are not in %s.\n\n"+
		"Nothing is wrong with the pin — this machine has never composed that base, "+
		"or its cache was cleaned. Re-compose it:\n\n%s\n\n"+
		"mgit does not re-fetch it for you: a tag can resolve to a different image "+
		"than the one you pinned, so a silent re-fetch would either fail this same "+
		"check or quietly boot something you did not choose",
		ErrBaseNotCached, name, entry.Digest, provenance, cacheRoot, recompose)
}

// sourceTag recovers the human-facing half of a provenance reference —
// "registry/repo:tag" out of "registry/repo:tag@sha256:..." — so the remedy we
// print is the command the user originally typed rather than a digest they
// never chose.
func sourceTag(source string) string {
	name, _, _ := strings.Cut(source, "@")
	return name
}

// InTreeBaseDir is where mgit before MGIT-147 unpacked a composed guest base:
// inside the repository, under the host config root. Nothing writes there any
// more; it is named here so the migration can find and collect it.
const InTreeBaseDir = "base"

// LegacyInTreeBase returns the in-tree base directory under hostRoot when one
// exists, or "" when the repository is already clean of base bytes.
func LegacyInTreeBase(hostRoot string) string {
	dir := filepath.Join(hostRoot, InTreeBaseDir)
	if info, err := os.Stat(dir); err == nil && info.IsDir() {
		return dir
	}
	return ""
}
