package main

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/hyper-swe/mgit/internal/docs"
)

// mgit's agent instructions live in one file mgit owns, MGIT.md, reached by
// one pointer line in each instruction file. Each tool writes only its own
// file: when HYPERSWE.md exists, the other tool owns the entry point and
// links MGIT.md from it, so mgit adds no pointer of its own. Refs: HSWE-19.3
const (
	mgitDocFile   = "MGIT.md"
	hyperswedFile = "HYPERSWE.md"
	claudePointer = "@MGIT.md"
	agentsPointer = "This project uses mgit for task-tagged commits and sandboxed work; see MGIT.md for instructions."
	claudeDocName = "CLAUDE.md"
	agentsDocName = "AGENTS.md"
)

// writeAgentEntry writes MGIT.md and, unless HYPERSWE.md exists, one pointer
// line in CLAUDE.md and AGENTS.md. It only ever adds a line: a pointer the
// user placed is never moved or removed, and a repeat run adds nothing.
func writeAgentEntry(root string, out io.Writer) error {
	want := docs.AgentInstructions()
	docPath := filepath.Join(root, mgitDocFile)
	if have, err := os.ReadFile(docPath); err != nil || string(have) != want { //nolint:gosec // path built from the repository root
		if err := os.WriteFile(docPath, []byte(want), 0o600); err != nil {
			return fmt.Errorf("write %s: %w", mgitDocFile, err)
		}
		_, _ = fmt.Fprintf(out, "Wrote %s\n", docPath)
	}
	if _, err := os.Stat(filepath.Join(root, hyperswedFile)); err == nil {
		return nil
	}
	for _, f := range []struct{ name, line string }{{claudeDocName, claudePointer}, {agentsDocName, agentsPointer}} {
		if err := addPointerLine(filepath.Join(root, f.name), f.line); err != nil {
			return err
		}
	}
	return nil
}

// addPointerLine appends line to the file (creating it) unless a line equal
// to it is already there.
func addPointerLine(path, line string) error {
	raw, err := os.ReadFile(path) //nolint:gosec // path built from the repository root
	if err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("read %s: %w", filepath.Base(path), err)
	}
	existing := string(raw)
	for _, l := range strings.Split(existing, "\n") {
		if strings.TrimSpace(l) == line {
			return nil
		}
	}
	var next string
	switch {
	case existing == "":
		next = line + "\n"
	case strings.HasSuffix(existing, "\n"):
		next = existing + line + "\n"
	default:
		next = existing + "\n" + line
	}
	if err := os.WriteFile(path, []byte(next), 0o600); err != nil { //nolint:gosec // path built from the repository root
		return fmt.Errorf("write %s: %w", filepath.Base(path), err)
	}
	return nil
}

// snapshotAgentFiles remembers MGIT.md, CLAUDE.md and AGENTS.md as they are,
// and returns a function that puts them back after a failed `mgit init`, which
// writes them ahead of the base. The rule is that nothing that was there
// before is ever removed: a path that did not exist is removed only if it is
// still a regular file init could have made; a path that existed and whose
// bytes were read (through a symlink, the target's) is rewritten to those bytes
// if it changed; a path that
// existed and could not be read (mode 000, a directory) is left alone, since
// init wrote nothing in it.
func snapshotAgentFiles(root string) func() {
	type saved struct {
		path    string
		existed bool
		read    bool
		data    []byte
		mode    os.FileMode
	}
	all := make([]saved, 0, 3)
	for _, name := range []string{mgitDocFile, claudeDocName, agentsDocName} {
		p := filepath.Join(root, name)
		s := saved{path: p}
		if _, err := os.Lstat(p); err == nil {
			s.existed = true
			// Stat and ReadFile follow a symlink: the bytes of the file the
			// link points at are what a write through the link changes, so
			// they are what is put back. A target that is a directory, is
			// missing or cannot be read stays unread, and is left alone.
			if st, err := os.Stat(p); err == nil && st.Mode().IsRegular() {
				if raw, err := os.ReadFile(p); err == nil { //nolint:gosec // path built from the repository root
					s.read, s.data, s.mode = true, raw, st.Mode().Perm()
				}
			}
		}
		all = append(all, s)
	}
	return func() {
		for _, s := range all {
			switch {
			case !s.existed:
				if st, err := os.Lstat(s.path); err == nil && st.Mode().IsRegular() {
					_ = os.Remove(s.path)
				}
			case s.read:
				if cur, err := os.ReadFile(s.path); err != nil || string(cur) != string(s.data) { //nolint:gosec // path built from the repository root
					_ = os.WriteFile(s.path, s.data, s.mode)
				}
			}
		}
	}
}
