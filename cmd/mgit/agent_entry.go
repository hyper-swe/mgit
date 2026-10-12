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
