package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// projectPath turns a path argument, as typed in the working directory, into
// the project-relative path mgit stages: relative to the working directory,
// as git resolves it, then relative to the project root.
//
// Paths used to be taken as root-relative wherever the command ran, so from
// pkg/ `mgit add a.go` failed to match, and `mgit add .` named the whole
// project rather than pkg/ (MGIT-278.1). Resolved here, `.` from pkg/ is
// "pkg", which `add` expands like git's `add .`, and `..` steps out of the
// subdirectory but never out of the project. Both sides are compared with
// symlinks resolved, because the working directory and the root can be the
// same directory under two spellings (macOS's /var is /private/var).
// Refs: MGIT-278.1
func projectPath(root, cwd, arg string) (string, error) {
	abs := arg
	if !filepath.IsAbs(abs) {
		abs = filepath.Join(cwd, arg)
	}
	rel, err := filepath.Rel(resolvedExisting(root), resolvedExisting(abs))
	if err != nil {
		return "", fmt.Errorf("%s: %w", arg, err)
	}
	rel = filepath.ToSlash(rel)
	if rel == ".." || strings.HasPrefix(rel, "../") {
		return "", fmt.Errorf("%s is outside the project (%s)", arg, root)
	}
	return rel, nil
}

// projectPaths resolves every argument with projectPath from the process's
// working directory.
func projectPaths(root string, args []string) ([]string, error) {
	cwd, err := os.Getwd()
	if err != nil {
		return nil, fmt.Errorf("get working directory: %w", err)
	}
	out := make([]string, 0, len(args))
	for _, a := range args {
		rel, err := projectPath(root, cwd, a)
		if err != nil {
			return nil, err
		}
		out = append(out, rel)
	}
	return out, nil
}

// resolvedExisting resolves symlinks in the deepest existing ancestor of p and
// appends the rest: a path being staged as a deletion no longer exists, and
// must still be spelled the way the root is.
func resolvedExisting(p string) string {
	p = filepath.Clean(p)
	rest := ""
	for dir := p; ; dir = filepath.Dir(dir) {
		if resolved, err := filepath.EvalSymlinks(dir); err == nil {
			return filepath.Join(resolved, rest)
		}
		if filepath.Dir(dir) == dir {
			return p
		}
		rest = filepath.Join(filepath.Base(dir), rest)
	}
}
