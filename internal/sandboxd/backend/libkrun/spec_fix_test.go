package libkrun

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The backend's own refusal of a base without its mount points must name a
// fix a reader can run IN THE TREE. It said `mkdir -p /proc /tmp`: the host's
// paths, which fix nothing in the base (and are not writable on macOS). The
// printed command is split into argv as sh would, every path must be inside
// the tree, and applying it must leave a base that validates. Refs: MGIT-249
func TestValidateGuestBase_MissingMountPoints_NamesARunnableFix(t *testing.T) {
	root := filepath.Join(t.TempDir(), "a base with spaces", "tree")
	for _, d := range []string{"sbin", "dev", "mnt"} {
		if err := os.MkdirAll(filepath.Join(root, d), 0o750); err != nil {
			t.Fatal(err)
		}
	}
	//nolint:gosec // G306: the guest PID-1 supervisor must be executable
	if err := os.WriteFile(filepath.Join(root, guestInitPath), []byte("#!/bin/true\n"), 0o750); err != nil {
		t.Fatal(err)
	}
	err := validateGuestBase(root, guestInitPath)
	if err == nil {
		t.Fatal("a base without /proc and /tmp cannot boot")
	}
	applyMkdirFix(t, err.Error(), root)
	if err := validateGuestBase(root, guestInitPath); err != nil {
		t.Fatalf("the printed fix must be the whole fix: %v", err)
	}
}

// applyMkdirFix finds the `mkdir -p …` line a refusal printed, splits it
// into argv the way sh would, requires every path to be inside root (a fix
// that names the HOST's /proc is not a fix for the tree), and applies it.
func applyMkdirFix(t *testing.T, msg, root string) {
	t.Helper()
	var line string
	for _, l := range strings.Split(msg, "\n") {
		if strings.HasPrefix(strings.TrimSpace(l), "mkdir -p ") {
			line = strings.TrimSpace(l)
		}
	}
	if line == "" {
		t.Fatalf("the refusal names no runnable `mkdir -p` line of its own: %q", msg)
	}
	argv := shellWords(t, line)
	if len(argv) < 3 || argv[0] != "mkdir" || argv[1] != "-p" {
		t.Fatalf("not a mkdir -p command: %q", line)
	}
	for _, p := range argv[2:] {
		if !strings.HasPrefix(p, root+string(filepath.Separator)) {
			t.Fatalf("the fix names %q, outside the tree %q: %q", p, root, line)
		}
		if err := os.MkdirAll(p, 0o750); err != nil {
			t.Fatal(err)
		}
	}
}

// shellWords splits a command line as a POSIX shell would for the forms a
// printed fix may use: bare words, single quotes, and backslash escapes.
func shellWords(t *testing.T, s string) []string {
	t.Helper()
	var words []string
	var cur strings.Builder
	inWord, quoted := false, false
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case quoted && c == '\'':
			quoted = false
		case quoted:
			cur.WriteByte(c)
		case c == '\'':
			quoted, inWord = true, true
		case c == '\\' && i+1 < len(s):
			i++
			cur.WriteByte(s[i])
			inWord = true
		case c == ' ' || c == '\t':
			if inWord {
				words = append(words, cur.String())
				cur.Reset()
				inWord = false
			}
		default:
			cur.WriteByte(c)
			inWord = true
		}
	}
	if quoted {
		t.Fatalf("unterminated quote in %q", s)
	}
	if inWord {
		words = append(words, cur.String())
	}
	return words
}
