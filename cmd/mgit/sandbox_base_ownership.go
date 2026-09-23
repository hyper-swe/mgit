package main

import (
	"fmt"

	"github.com/hyper-swe/mgit/internal/sandboxd/basecache"
)

// refuseCachedBaseTree refuses to pin, and so to write into, a directory
// inside an mgit guest-base cache.
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
// The test is identity, not spelling (basecache.Owner): a case variant, a
// firmlink, or a symlink names the same directory. The known roots are this
// process's cache and the machine-wide one with no override applied, so a
// process pointed elsewhere still recognizes the cache every other repository
// uses. Any other root is recognized by the marker it carries. A cache
// location that cannot be determined is a refusal, never a pass: "cannot tell
// whose tree this is" must not read as "not the cache's". Refs: MGIT-226, MGIT-147
func refuseCachedBaseTree(baseDir string) error {
	root, err := basecache.DefaultRoot()
	if err != nil {
		return fmt.Errorf("base set: cannot tell whether %s is inside mgit's base cache: %w", baseDir, err)
	}
	known := []string{root}
	// No OS cache directory means no machine-wide cache exists to protect;
	// this process's own root is still checked above it.
	if sys, sysErr := basecache.SystemRoot(); sysErr == nil {
		known = append(known, sys)
	}
	owner, err := basecache.Owner(baseDir, known)
	if err != nil {
		return fmt.Errorf("base set: cannot tell whether %s is inside mgit's base cache: %w", baseDir, err)
	}
	if owner == "" {
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
			"  mgit sandbox base from", baseDir, owner, baseDir)
}
