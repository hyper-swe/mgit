package main

import (
	"fmt"
	"path/filepath"
	"strings"

	"github.com/hyper-swe/mgit/internal/sandboxd/basecache"
)

// refuseCachedBaseTree refuses to pin, and so to write into, a directory
// inside mgit's own guest-base cache.
//
// `base set` writes the guest payload INTO the tree it is given, because the
// pin must cover what boots. A cache entry is named by its content and pinned
// by every repository that composed it. Writing into one rewrites another
// repository's base under its key, and every launch pinned to that key fails
// verification. That happened (MGIT-226): a scratch repository's `base set`
// rewrote a base another working directory had measured on. The entry is
// refused and never touched. Whoever wants that userspace registers a COPY,
// or composes it.
//
// Both sides are resolved through symlinks before they are compared, so a
// link pointing into the cache is the cache. A cache root that cannot be
// located is a refusal, not a pass: "cannot tell whose tree this is" must not
// read as "not the cache's". Refs: MGIT-226, MGIT-147
func refuseCachedBaseTree(baseDir string) error {
	root, err := basecache.DefaultRoot()
	if err != nil {
		return fmt.Errorf("base set: cannot tell whether %s is inside mgit's base cache: %w", baseDir, err)
	}
	dir, cacheRoot := resolvedPath(baseDir), resolvedPath(root)
	// A path Rel cannot express (a different volume) is outside by definition,
	// so it is folded into the same answer rather than raised.
	rel, relErr := filepath.Rel(cacheRoot, dir)
	outside := relErr != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator))
	if outside {
		return nil
	}
	return fmt.Errorf(
		"guest base %s is inside mgit's base cache %s.\n\n"+
			"A cache entry is named by its content and pinned by every repository that composed it. "+
			"`base set` writes the guest payload into the tree it is given, so pinning this entry would "+
			"rewrite another repository's base under its key, and every launch pinned to it would fail "+
			"verification.\n\n"+
			"To use that userspace, copy it outside the cache and set the copy, or compose it:\n"+
			"  cp -R %s <dir> && mgit sandbox base set <dir>\n"+
			"  mgit sandbox base from", baseDir, root, dir)
}

// resolvedPath follows symlinks where the path exists and falls back to the
// cleaned path where it does not: a path that is not there yet cannot be a
// link into anything.
func resolvedPath(p string) string {
	if r, err := filepath.EvalSymlinks(p); err == nil {
		return r
	}
	return filepath.Clean(p)
}
