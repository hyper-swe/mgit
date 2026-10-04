package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

// A refusal that names its fix must name one a reader can run. The printed
// command is split into argv the way sh would, every path in it must be
// inside the tree, and applying it must leave a tree that validates. The old
// form printed `mkdir -p <tree>[/proc /dev /tmp /mnt]`: a Go slice glued to
// the path, which no shell runs. The tree's path has spaces, so the fix must
// quote. Refs: MGIT-249
func TestValidateBaseTree_MissingMountPoints_NamesARunnableFix(t *testing.T) {
	root := filepath.Join(t.TempDir(), "a base with spaces", "tree")
	require.NoError(t, os.MkdirAll(filepath.Join(root, "dev"), 0o750))
	require.NoError(t, os.MkdirAll(filepath.Join(root, "mnt"), 0o750))
	err := validateBaseTree(root)
	require.Error(t, err, "a tree without /proc and /tmp cannot boot")
	applyMkdirFix(t, err.Error(), root)
	require.NoError(t, validateBaseTree(root), "the printed fix must be the whole fix")
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
