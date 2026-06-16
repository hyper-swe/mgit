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
	Mounts       []Mount
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
