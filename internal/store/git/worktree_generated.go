package git

import (
	"bufio"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// This file carries mgit's answer to MGIT-80: mgit itself writes agent
// scaffolding INTO a worktree (`mgit work` generates the CLAUDE.md sandbox
// block, `.claude/settings.json`, `AGENTS.md`, `.envrc`, the Cursor rule), and
// none of it is the task's work. Without a record of that provenance a bulk
// stage (`mgit add -A`, `mgit add .`, `mgit commit -a`) sweeps mgit's own files
// into the task branch and into the patch that lands in the user's repository —
// where the content is not merely noise but WRONG, since it describes one
// worktree's task binding and this host's sandbox availability.
//
// The mechanism is a per-worktree manifest of PROVENANCE — the paths mgit
// generated here — written at generation time under the worktree's own .mgit/
// (already excluded from staging) and consulted by the bulk-staging walk.
//
// Why provenance rather than the obvious alternatives:
//   - A hard-coded filename blocklist would encode "CLAUDE.md is mgit's", which
//     is false: plenty of projects track their own CLAUDE.md/AGENTS.md/.envrc
//     and must still be able to bulk-stage edits to them.
//   - Appending to the project's .gitignore writes into USER content, and that
//     write would itself land in the patch — the same defect one level up.
//   - A .git/info/exclude-style ignore file fed to Repository.ignoreMatcher
//     (MGIT-32) only covers UNTRACKED paths: gitignore semantics do not retract
//     a tracked file, and dropping a tracked path from listWorkingFiles would
//     make Status report it DELETED — staging the removal of the user's real
//     CLAUDE.md, a strictly worse bug.
//
// Applying provenance at bulk-stage time is orthogonal to tracked/untracked, so
// both shapes (new file, and edit to a tracked file) fall out of one rule, and
// an explicit `mgit add <path>` still stages the file — a named pathspec is an
// unambiguous statement of intent, mirroring `git add -f`.
// Refs: MGIT-80, MGIT-77, FR-16

// generatedFileName is the per-worktree manifest, under the worktree's .mgit/,
// listing the project-relative paths mgit generated into that worktree. Same
// newline/`#`-comment format as seed-include so the store's small config files
// read alike. Refs: MGIT-80
const generatedFileName = "generated"

// generatedManifestPath returns the manifest path for a worktree root.
func generatedManifestPath(worktreeRoot string) string {
	return filepath.Join(worktreeRoot, mgitDirName, generatedFileName)
}

// RecordGeneratedPaths records project-relative paths as mgit-generated in the
// worktree at worktreeRoot, merging with anything already recorded (idempotent,
// sorted, deduplicated) so re-running `mgit work` or relaunching a sandbox
// cannot grow the manifest without bound. Every path is validated: an entry
// that is empty, absolute, parent-escaping, or under .git/.mgit is a bug in a
// caller (the list is mgit-authored, never user input), so it is REFUSED and
// nothing is written. Refs: MGIT-80, MGIT-14.7
func RecordGeneratedPaths(worktreeRoot string, rels []string) error {
	cleaned := make([]string, 0, len(rels))
	for _, rel := range rels {
		norm := filepath.ToSlash(filepath.Clean(rel))
		if err := validateRelPath(rel); err != nil {
			return fmt.Errorf("record generated path %q: %w", rel, err)
		}
		cleaned = append(cleaned, norm)
	}
	if len(cleaned) == 0 {
		return nil
	}
	existing, err := ReadGeneratedPaths(worktreeRoot)
	if err != nil {
		return err
	}
	union := make(map[string]bool, len(existing)+len(cleaned))
	for _, p := range append(existing, cleaned...) {
		union[p] = true
	}
	merged := make([]string, 0, len(union))
	for p := range union {
		merged = append(merged, p)
	}
	sort.Strings(merged)
	return writeGeneratedManifest(worktreeRoot, merged)
}

// writeGeneratedManifest writes the manifest atomically (temp file + rename) so
// a crash mid-write can never leave a truncated list that silently stops
// excluding scaffolding. Refs: MGIT-80
func writeGeneratedManifest(worktreeRoot string, paths []string) error {
	return writeManifestIn(filepath.Join(worktreeRoot, mgitDirName), paths)
}

// writeManifestIn writes the manifest into the store directory dir, the
// directory a worktree sees as its .mgit.
func writeManifestIn(dir string, paths []string) error {
	if err := os.MkdirAll(dir, 0o750); err != nil {
		return fmt.Errorf("generated manifest: mkdir %s: %w", dir, err)
	}
	var b strings.Builder
	b.WriteString("# Files mgit generated into this worktree (MGIT-80).\n")
	b.WriteString("# Excluded from bulk staging; `mgit add <path>` still stages them.\n")
	for _, p := range paths {
		b.WriteString(p)
		b.WriteString("\n")
	}
	final := filepath.Join(dir, generatedFileName)
	tmp := final + ".tmp"
	if err := os.WriteFile(tmp, []byte(b.String()), 0o600); err != nil {
		return fmt.Errorf("generated manifest: write: %w", err)
	}
	if err := os.Rename(tmp, final); err != nil {
		return fmt.Errorf("generated manifest: commit write: %w", err)
	}
	return nil
}

// ReadGeneratedPaths returns the project-relative paths recorded as
// mgit-generated for the worktree at worktreeRoot, in file order. A missing
// manifest yields no paths and no error (the pre-MGIT-80 behavior: nothing is
// excluded). Reading is DEFENSIVE — comments, blanks, and entries that fail
// path validation are skipped rather than raising — because a hand-edited or
// truncated manifest must degrade to "exclude less", never to a commit that
// cannot be made. Refs: MGIT-80
func ReadGeneratedPaths(worktreeRoot string) ([]string, error) {
	f, err := os.Open(generatedManifestPath(worktreeRoot)) //nolint:gosec // fixed store-local config path
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("open generated manifest: %w", err)
	}
	defer f.Close() //nolint:errcheck // read-only file, close error is non-actionable
	return parseGeneratedManifest(f)
}

// ReadGeneratedPathsNoFollow reads the worktree's generated list for a
// sandbox launch, which copies it into the guest's private store (MGIT-236).
// Whatever this read reaches on the host reaches the guest, so it follows NO
// link: the worktree's .mgit must be a real directory and the list a regular
// file in it, or the read is REFUSED. The directory is pinned open (os.Root)
// and checked to be the one inspected, and so is the file, so neither can be
// swapped for a link between the check and the read. A missing .mgit or list
// yields no paths and no error, as for ReadGeneratedPaths.
// Refs: MGIT-236, MGIT-80, SEC-03
func ReadGeneratedPathsNoFollow(worktreeRoot string) ([]string, error) {
	root, err := openStoreDirNoFollow(filepath.Join(worktreeRoot, mgitDirName))
	if err != nil || root == nil {
		return nil, err
	}
	defer root.Close() //nolint:errcheck // read-only directory handle, close error is non-actionable
	path := filepath.Join(worktreeRoot, mgitDirName, generatedFileName)
	inspected, err := root.Lstat(generatedFileName)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("inspect generated list %s: %w", path, err)
	}
	if !inspected.Mode().IsRegular() {
		return nil, fmt.Errorf("the generated list %s is not a regular file (%s); refusing to follow it", path, inspected.Mode().Type())
	}
	f, err := root.Open(generatedFileName)
	if err != nil {
		return nil, fmt.Errorf("open generated list %s: %w", path, err)
	}
	defer f.Close() //nolint:errcheck // read-only file, close error is non-actionable
	if opened, err := f.Stat(); err != nil || !os.SameFile(inspected, opened) {
		return nil, fmt.Errorf("the generated list %s was replaced while it was read; refusing it", path)
	}
	return parseGeneratedManifest(f)
}

// openStoreDirNoFollow opens dir, a worktree's .mgit, as a Root pinned to
// the directory that was inspected. A missing dir is (nil, nil); anything
// but a real directory, a symlink included, is refused.
func openStoreDirNoFollow(dir string) (*os.Root, error) {
	inspected, err := os.Lstat(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("inspect %s: %w", dir, err)
	}
	if !inspected.IsDir() {
		return nil, fmt.Errorf("%s is not a directory (%s); refusing to read the generated list through it", dir, inspected.Mode().Type())
	}
	root, err := os.OpenRoot(dir)
	if err != nil {
		return nil, fmt.Errorf("open %s: %w", dir, err)
	}
	if opened, err := root.Stat("."); err != nil || !os.SameFile(inspected, opened) {
		_ = root.Close() // refusing; the handle's close error changes nothing
		return nil, fmt.Errorf("%s was replaced while it was read; refusing it", dir)
	}
	return root, nil
}

// WriteGeneratedPathsInStore writes paths as the generated list of the store
// directory storeDir: the directory a sandbox guest sees as its worktree's
// .mgit (MGIT-236). An empty list writes nothing. Refs: MGIT-236, MGIT-80
func WriteGeneratedPathsInStore(storeDir string, paths []string) error {
	if len(paths) == 0 {
		return nil
	}
	return writeManifestIn(storeDir, paths)
}

// parseGeneratedManifest reads the manifest format DEFENSIVELY (see
// ReadGeneratedPaths).
func parseGeneratedManifest(r io.Reader) ([]string, error) {
	var paths []string
	sc := bufio.NewScanner(r)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		rel := filepath.ToSlash(filepath.Clean(line))
		if validateRelPath(rel) != nil {
			continue
		}
		paths = append(paths, rel)
	}
	if err := sc.Err(); err != nil {
		return nil, fmt.Errorf("read generated manifest: %w", err)
	}
	return paths, nil
}

// IsGenerated reports whether a project-relative path was recorded as
// mgit-generated in this worktree. Staging such a path explicitly is allowed —
// a named pathspec is an unambiguous statement of intent — so the CLI uses this
// to WARN rather than refuse: the file's content includes mgit's generated
// block, and the caller should know that before it lands. Refs: MGIT-80
func (ws *WorktreeStore) IsGenerated(rel string) (bool, error) {
	generated, err := ws.repo.generatedSet()
	if err != nil {
		return false, err
	}
	return generated[filepath.ToSlash(filepath.Clean(rel))], nil
}

// generatedSet returns this repository's mgit-generated paths as a lookup set.
// For a linked worktree the Repository root IS the worktree root, so the
// manifest read is per-worktree — exactly the scope the provenance describes.
// Refs: MGIT-80, FR-16
func (r *Repository) generatedSet() (map[string]bool, error) {
	paths, err := ReadGeneratedPaths(r.root)
	if err != nil {
		return nil, err
	}
	set := make(map[string]bool, len(paths))
	for _, p := range paths {
		set[p] = true
	}
	return set, nil
}
