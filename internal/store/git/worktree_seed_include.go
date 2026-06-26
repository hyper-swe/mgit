package git

import (
	"bufio"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
)

// seedIncludeFileName is the store-local config (under .mgit/) listing
// newline-separated globs of gitignored-but-build-required working-tree paths
// that must still be carried into a freshly materialized worktree. These paths
// were never imported into .mgit (mgit's `add` honors .gitignore, MGIT-32), so
// they are absent from the materialized object tree and would otherwise be
// missing — breaking a fresh worktree's `go build ./...` for generated,
// //go:embed-ed artifacts such as web/dist. Refs: MGIT-38
const seedIncludeFileName = "seed-include"

// seedPathSafe reports whether a project-relative path is safe to carry as a
// seed-include: it must pass validateRelPath (non-empty, not absolute, not
// parent-escaping, not under .git/.mgit). It wraps the error-returning check as
// a boolean so the silent-skip callers read clearly. Refs: MGIT-38
func seedPathSafe(rel string) bool {
	return validateRelPath(rel) == nil
}

// readSeedIncludes reads .mgit/seed-include and returns the list of non-blank,
// non-comment globs (leading/trailing whitespace trimmed, `#` lines ignored). A
// missing file yields no includes, preserving the pre-MGIT-38 behavior of
// carrying nothing beyond the materialized tree. Refs: MGIT-38
func (r *Repository) readSeedIncludes() ([]string, error) {
	path := filepath.Join(r.MgitDir(), seedIncludeFileName)
	f, err := os.Open(path) //nolint:gosec // path is the fixed store-local config file
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("open seed-include: %w", err)
	}
	defer f.Close() //nolint:errcheck // read-only file, close error is non-actionable
	var globs []string
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		globs = append(globs, filepath.ToSlash(line))
	}
	if err := sc.Err(); err != nil {
		return nil, fmt.Errorf("read seed-include: %w", err)
	}
	return globs, nil
}

// copySeedIncludes copies, from the live SOURCE working tree (r.Root()) into
// destRoot at the same relative path, every working-tree file matching a
// seed-include glob. It runs AFTER MaterializeBranchTo has written the .mgit
// tree, deliberately carrying gitignored-but-build-required artifacts that are
// NOT in the object store (so the base/audit stays clean — build artifacts are
// ephemeral). Each candidate path is validated with validateRelPath so .git,
// .mgit, absolute, and parent-escaping globs are rejected and can never write
// outside destRoot. A glob matching nothing in the source is skipped silently
// (the path is optional). Refs: MGIT-38
func (r *Repository) copySeedIncludes(destRoot string) error {
	globs, err := r.readSeedIncludes()
	if err != nil {
		return err
	}
	for _, glob := range globs {
		matches, err := r.matchSeedGlob(glob)
		if err != nil {
			return err
		}
		for _, rel := range matches {
			if err := r.copyWorkingFileToDir(destRoot, rel); err != nil {
				return err
			}
		}
	}
	return nil
}

// matchSeedGlob expands a single seed-include glob against the SOURCE working
// tree and returns the project-relative FILE paths it carries. A glob naming a
// directory (e.g. web/dist) carries that whole subtree; a `**` suffix (e.g.
// web/dist/**) is treated the same way (every file under the prefix);
// otherwise filepath.Match is applied per top-level-walked file. Any candidate
// that fails validateRelPath (absolute, traversal, or .git/.mgit) is dropped,
// so a malicious glob matches nothing rather than escaping. A glob with no
// match returns an empty slice (skipped silently). Refs: MGIT-38
func (r *Repository) matchSeedGlob(glob string) ([]string, error) {
	clean := filepath.ToSlash(filepath.Clean(strings.TrimSuffix(glob, "/")))
	// Reject unsafe globs up front: an escaping/excluded prefix carries nothing.
	if !seedPathSafe(clean) {
		return nil, nil
	}
	prefix := strings.TrimSuffix(clean, "/**")
	prefix = strings.TrimSuffix(prefix, "/*")
	// A directory (or `prefix/**`) carries its entire subtree.
	if info, err := os.Lstat(filepath.Join(r.root, filepath.FromSlash(prefix))); err == nil && info.IsDir() {
		return r.walkSeedSubtree(prefix)
	}
	return r.matchSeedFiles(clean)
}

// walkSeedSubtree returns every project-relative file path under prefix in the
// source working tree (recursively). validateRelPath is re-checked per file so
// nothing under an excluded root is ever returned. Refs: MGIT-38
func (r *Repository) walkSeedSubtree(prefix string) ([]string, error) {
	base := filepath.Join(r.root, filepath.FromSlash(prefix))
	var out []string
	err := filepath.WalkDir(base, func(abs string, d fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if d.IsDir() {
			return nil
		}
		rel, err := filepath.Rel(r.root, abs)
		if err != nil {
			return err
		}
		rel = filepath.ToSlash(rel)
		if seedPathSafe(rel) {
			out = append(out, rel)
		}
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("walk seed subtree %s: %w", prefix, err)
	}
	return out, nil
}

// matchSeedFiles matches a non-directory glob against every file in the source
// working tree via filepath.Match (on the project-relative slash path), keeping
// only validateRelPath-safe matches. Refs: MGIT-38
func (r *Repository) matchSeedFiles(glob string) ([]string, error) {
	var out []string
	err := filepath.WalkDir(r.root, func(abs string, d fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		rel, err := filepath.Rel(r.root, abs)
		if err != nil {
			return err
		}
		rel = filepath.ToSlash(rel)
		if rel == "." {
			return nil
		}
		top := strings.SplitN(rel, "/", 2)[0]
		if excludedRoots[top] {
			if d.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		if d.IsDir() {
			return nil
		}
		ok, matchErr := filepath.Match(glob, rel)
		if matchErr != nil {
			return fmt.Errorf("match seed glob %q: %w", glob, matchErr)
		}
		if ok && seedPathSafe(rel) {
			out = append(out, rel)
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

// copyWorkingFileToDir copies a single SOURCE working-tree file (by validated
// project-relative path) into destRoot at the same relative path, creating
// parent dirs and preserving the file's permission mode. A symlink is recreated
// as a symlink (its link text), never dereferenced — matching workingFileContent
// so a seed-include glob cannot exfiltrate an out-of-tree target's content. A
// source path absent on disk is skipped silently. Refs: MGIT-38
func (r *Repository) copyWorkingFileToDir(destRoot, rel string) error {
	if err := validateRelPath(rel); err != nil {
		return err
	}
	src := filepath.Join(r.root, filepath.FromSlash(rel))
	info, err := os.Lstat(src)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return fmt.Errorf("stat seed file %s: %w", rel, err)
	}
	dst := filepath.Join(destRoot, filepath.FromSlash(rel))
	if err := os.MkdirAll(filepath.Dir(dst), 0o750); err != nil {
		return fmt.Errorf("mkdir for seed file %s: %w", rel, err)
	}
	if info.Mode()&os.ModeSymlink != 0 {
		target, rerr := os.Readlink(src)
		if rerr != nil {
			return fmt.Errorf("read seed symlink %s: %w", rel, rerr)
		}
		if rmErr := os.Remove(dst); rmErr != nil && !os.IsNotExist(rmErr) {
			return fmt.Errorf("replace seed file %s: %w", rel, rmErr)
		}
		if lErr := os.Symlink(target, dst); lErr != nil {
			return fmt.Errorf("write seed symlink %s: %w", rel, lErr)
		}
		return nil
	}
	data, err := os.ReadFile(src) //nolint:gosec // rel validated against root and excluded dirs
	if err != nil {
		return fmt.Errorf("read seed file %s: %w", rel, err)
	}
	//nolint:gosec // dst = destRoot + rel; rel is validated by validateRelPath
	// (rejects absolute/traversal/.git/.mgit) so it cannot escape destRoot.
	if err := os.WriteFile(dst, data, info.Mode().Perm()); err != nil {
		return fmt.Errorf("write seed file %s: %w", rel, err)
	}
	return nil
}
