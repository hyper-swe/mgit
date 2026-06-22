// Package quarantine computes the host-side guest-filesystem plan and
// the land-time host-trusted-path defense for the FR-17 sandbox. The
// guest is the hostile party: it may write anything to its read-write
// mounts, so the authoritative guarantees are enforced host-side — the
// plan decides what is even visible (worktree only, FR-17.3) and what is
// read-only (host-trusted paths, FR-17.14), and the land check refuses
// any commit that modified a host-trusted path (T8). This package is
// pure Go and host-only; backends consume the plan to configure mounts.
// Refs: FR-17.3, FR-17.4, FR-17.14, SEC-03
package quarantine

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/hyper-swe/mgit/internal/model"
)

// guestStoreName is the directory, relative to the worktree, where the guest's
// mgit finds its self-contained object store. Per the ADR-001 amendment
// (2026-06-22, MGIT-14) mgit's store lives in `.mgit/` and mgit never owns or
// writes a `.git` at the worktree root, so the SEC-03 private store is bound
// here — not at a `.git` gitfile that no longer exists. Refs: SEC-03, MGIT-14
const guestStoreName = ".mgit"

// Mount is one host→guest filesystem mapping in the plan.
type Mount struct {
	HostPath  string // absolute host source (always within the worktree, FR-17.3)
	GuestPath string // absolute guest target; equals HostPath for the worktree (no translation)
	ReadOnly  bool
}

// Plan is the complete guest filesystem view: the worktree mounted
// read-write at its identical absolute path, with existing host-trusted
// paths layered read-only on top. Nothing outside the worktree is ever
// included — no host $HOME, no parent repository, no shared object
// store (FR-17.3). The first mount is always the worktree.
type Plan struct {
	WorktreePath string
	// PrivateStorePath is the host directory backing the guest's private,
	// sandbox-local mgit object store (mounted at the guest's .mgit), set by
	// BindPrivateStore. Empty until bound. The shared store is never part of
	// the plan (SEC-03).
	PrivateStorePath string
	Mounts           []Mount
}

// BuildPlan computes the guest filesystem plan for a worktree. The
// worktree path MUST be absolute (the guest mounts at the identical
// absolute path). Only host-trusted paths that actually exist under the
// worktree get a read-only mount — no phantom mounts. Refs: FR-17.3, FR-17.14
func BuildPlan(worktreePath string, sensitive []string) (Plan, error) {
	if !filepath.IsAbs(worktreePath) {
		return Plan{}, fmt.Errorf("quarantine: worktree path must be absolute, got %q", worktreePath)
	}
	clean := filepath.Clean(worktreePath)
	plan := Plan{
		WorktreePath: clean,
		// The worktree is the only read-write mount, at the identical path.
		Mounts: []Mount{{HostPath: clean, GuestPath: clean, ReadOnly: false}},
	}
	for _, pattern := range sensitive {
		rel := normalizePattern(pattern)
		if rel == "" {
			continue
		}
		host := filepath.Join(clean, filepath.FromSlash(rel))
		if _, err := os.Lstat(host); err != nil {
			continue // a host-trusted path that does not exist needs no mount
		}
		plan.Mounts = append(plan.Mounts, Mount{HostPath: host, GuestPath: host, ReadOnly: true})
	}
	return plan, nil
}

// BindPrivateStore adds the SEC-03 private object store to the plan: the
// guest's mgit store (.mgit) is mapped read-write to a sandbox-local store
// (privateStoreDir) at the worktree's identical-path .mgit, so guest
// commits go to the private store and the host shared store
// (sharedStoreDir — .mgit objects/refs/index) is never mounted and never
// resolvable from inside the guest. Only `mgit sandbox land` bridges the
// private store to the shared one.
//
// The guest target is .mgit, not .git: per the ADR-001 amendment (MGIT-14)
// mgit keeps a self-contained .mgit store and never owns a .git at the
// worktree root, so there is no `<worktree>/.git` to bind — the private store
// is sourced independently of any project/worktree .git.
//
// It enforces the layout invariants that make the guarantee real: the
// shared store must sit OUTSIDE the mounted worktree (else the guest would
// see it as a worktree file), the private store must sit OUTSIDE the
// worktree (it is sandbox-local, not a guest-editable file), and the
// private store must not resolve into the shared store.
//
// Errors are classified by who must act: a breach that would make the
// shared store reachable from the guest returns the ErrSharedStoreReachable
// sentinel (callers can errors.Is it to reject the launch as a quarantine
// failure); a caller-side input mistake (a non-absolute or misplaced
// private-store path) returns a plain error. Refs: SEC-03, FR-17.3, FR-17.5
func (p Plan) BindPrivateStore(privateStoreDir, sharedStoreDir string) (Plan, error) {
	if !filepath.IsAbs(privateStoreDir) {
		return Plan{}, fmt.Errorf("quarantine: private store dir must be absolute, got %q", privateStoreDir)
	}
	if !filepath.IsAbs(sharedStoreDir) {
		return Plan{}, fmt.Errorf("quarantine: shared store dir must be absolute, got %q", sharedStoreDir)
	}
	priv, shared := filepath.Clean(privateStoreDir), filepath.Clean(sharedStoreDir)

	if isWithin(shared, p.WorktreePath) {
		return Plan{}, fmt.Errorf("%w: shared store %q is inside the mounted worktree %q",
			model.ErrSharedStoreReachable, shared, p.WorktreePath)
	}
	if isWithin(priv, p.WorktreePath) {
		return Plan{}, fmt.Errorf("quarantine: private store %q must be outside the worktree %q", priv, p.WorktreePath)
	}
	if isWithin(priv, shared) {
		return Plan{}, fmt.Errorf("%w: private store %q resolves into the shared store %q",
			model.ErrSharedStoreReachable, priv, shared)
	}

	// The private store fully owns the guest's .mgit store subtree. Build a
	// fresh mount list (never mutating the receiver's backing array) that
	// drops any mount BuildPlan layered inside .mgit: no host .mgit/* may
	// resolve inside the guest's private store, which the private store, not
	// the host worktree, owns. A worktree's own .git (the project working
	// tree, e.g. a host-trusted .git/hooks) is untouched — it is not mgit's
	// store. Refs: SEC-03, FR-17.14
	storeGuestPath := filepath.Join(p.WorktreePath, guestStoreName)
	mounts := make([]Mount, 0, len(p.Mounts)+1)
	for _, m := range p.Mounts {
		if isWithin(m.GuestPath, storeGuestPath) {
			continue // superseded by the private store
		}
		mounts = append(mounts, m)
	}
	mounts = append(mounts, Mount{HostPath: priv, GuestPath: storeGuestPath, ReadOnly: false})
	p.Mounts, p.PrivateStorePath = mounts, priv

	// Defense in depth: no mount may resolve into the shared store.
	for _, m := range p.Mounts {
		if isWithin(m.HostPath, shared) {
			return Plan{}, fmt.Errorf("%w: mount %q is inside the shared store %q",
				model.ErrSharedStoreReachable, m.HostPath, shared)
		}
	}
	return p, nil
}

// isWithin reports whether path is dir or nested under dir (both cleaned,
// absolute). Used to enforce the SEC-03 containment invariants.
func isWithin(path, dir string) bool {
	if path == dir {
		return true
	}
	rel, err := filepath.Rel(dir, path)
	if err != nil {
		return false
	}
	return rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)) && !filepath.IsAbs(rel)
}

// normalizePattern returns the canonical slash-separated, slash-trimmed
// form of a host-trusted pattern. Shared by the mount builder and the
// land check so both halves of the FR-17.14 guarantee agree on exactly
// what a pattern matches.
func normalizePattern(pattern string) string {
	return strings.Trim(filepath.ToSlash(pattern), "/")
}

// WorktreeMount returns the worktree's read-write mount (always present
// after BuildPlan, since WorktreePath is cleaned there).
func (p Plan) WorktreeMount() *Mount {
	return p.mountFor(p.WorktreePath)
}

// mountFor returns the mount whose HostPath equals host, or nil.
func (p Plan) mountFor(host string) *Mount {
	want := filepath.Clean(host)
	for i := range p.Mounts {
		if p.Mounts[i].HostPath == want {
			return &p.Mounts[i]
		}
	}
	return nil
}

// IsSensitive reports whether a worktree-relative, slash-separated path
// falls under any host-trusted pattern. A directory pattern matches that
// directory and its whole subtree; a file pattern matches the exact
// path. Matching is by path segment, so "src/.clauderc" does not match
// ".claude/" and "docs/CLAUDE.md.txt" does not match "CLAUDE.md".
// Refs: FR-17.14
func IsSensitive(relPath string, patterns []string) bool {
	return matchesAny(cleanRel(relPath), normalizePatterns(patterns))
}

// CheckModifications returns ErrSensitivePathModified if any
// worktree-relative modified path is host-trusted. This is the
// authoritative land-time defense (FR-17.14): even though host-trusted
// paths are mounted read-only, a guest could attempt modification by
// other means, so land refuses the commit set rather than trusting the
// mount. The pattern list is normalized once, not per modified path.
// Refs: FR-17.14, T8
func CheckModifications(modified []string, patterns []string) error {
	norm := normalizePatterns(patterns)
	for _, path := range modified {
		if matchesAny(cleanRel(path), norm) {
			return fmt.Errorf("%w: %s", model.ErrSensitivePathModified, path)
		}
	}
	return nil
}

// cleanRel canonicalizes a worktree-relative path to slash-separated form.
func cleanRel(relPath string) string {
	return filepath.ToSlash(filepath.Clean(relPath))
}

// normalizePatterns returns the non-empty normalized forms of patterns.
func normalizePatterns(patterns []string) []string {
	out := make([]string, 0, len(patterns))
	for _, p := range patterns {
		if n := normalizePattern(p); n != "" {
			out = append(out, n)
		}
	}
	return out
}

// matchesAny reports whether a cleaned relative path equals or is nested
// under any already-normalized pattern.
func matchesAny(clean string, normPatterns []string) bool {
	for _, p := range normPatterns {
		if clean == p || strings.HasPrefix(clean, p+"/") {
			return true
		}
	}
	return false
}
