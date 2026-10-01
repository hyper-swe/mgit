// Package e2e — every command, on the real binary, refuses a stray argument
// OUT LOUD (MGIT-284). The unit walk proves each command's Args rule returns
// an error; it cannot see whether the user is told. `mgit verify` silences
// cobra's error printing, so its refusal exited 1 with nothing on stdout or
// stderr. This walks the binary's own help tree and runs every command that
// documents no positional argument with one, asserting a non-zero exit and a
// refusal on stderr that names the argument. Refs: MGIT-284, MGIT-282
package e2e

import (
	"bytes"
	"context"
	"os/exec"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// availableCommand matches one row of a help page's "Available Commands:".
var availableCommand = regexp.MustCompile(`^  ([a-z][a-z0-9-]*)\s{2,}`)

// helpOf runs `mgit <path...> --help` and returns its output.
func helpOf(t *testing.T, bin, dir string, path []string) string {
	t.Helper()
	out, err := runMgit(t, bin, dir, append(append([]string{}, path...), "--help")...)
	require.NoError(t, err, "help for %v: %s", path, out)
	return out
}

// subcommands lists the rows of a help page's "Available Commands:".
func subcommands(help string) []string {
	var names []string
	in := false
	for _, line := range strings.Split(help, "\n") {
		switch {
		case strings.HasPrefix(line, "Available Commands:"):
			in = true
		case in && strings.TrimSpace(line) == "":
			in = false
		case in:
			if m := availableCommand.FindStringSubmatch(line); m != nil {
				names = append(names, m[1])
			}
		}
	}
	return names
}

// documentsNoPositional reports whether a help page's usage line for the
// command itself shows nothing but flags: "mgit squash [flags]".
func documentsNoPositional(help string, path []string) bool {
	want := "mgit " + strings.Join(path, " ")
	for _, line := range strings.Split(help, "\n") {
		l := strings.TrimSpace(line)
		if !strings.HasPrefix(l, want) {
			continue
		}
		return onlyFlags(strings.Fields(strings.TrimPrefix(l, want)))
	}
	return false // no runnable usage line: a parent command
}

// onlyFlags reports whether usage words name only flags and their values:
// "[flags]", "--task <id>", "[--allow ...]". A placeholder right after a flag
// is that flag's value, not a positional argument.
func onlyFlags(words []string) bool {
	for i := 0; i < len(words); i++ {
		w := words[i]
		switch {
		case w == "[flags]" || w == "...]" || w == "[...]":
		case w == "--":
			return false
		case strings.HasPrefix(w, "--") || strings.HasPrefix(w, "[--"):
			if i+1 < len(words) && strings.HasPrefix(words[i+1], "<") {
				i++
			}
		default:
			return false
		}
	}
	return true
}

func TestE2E_EveryCommandRefusesAStrayArgumentOutLoud(t *testing.T) {
	bin := buildMgitBinary(t)
	repo := t.TempDir()
	mustMgit(t, bin, repo, "init")

	var leaves [][]string
	var walk func(path []string)
	walk = func(path []string) {
		help := helpOf(t, bin, repo, path)
		for _, name := range subcommands(help) {
			if name == "help" || name == "completion" {
				continue
			}
			walk(append(append([]string{}, path...), name))
		}
		if len(path) > 0 && documentsNoPositional(help, path) {
			leaves = append(leaves, path)
		}
	}
	walk(nil)
	// The walk must reach the tree, not stop at the root: a vacuous walk
	// would pass by checking nothing.
	require.Greater(t, len(leaves), 15, "the help walk found only %d commands", len(leaves))
	t.Logf("checked %d commands that document no positional argument", len(leaves))

	for _, path := range leaves {
		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		cmd := exec.CommandContext(ctx, bin, append(append([]string{}, path...), "zzstray")...) //nolint:gosec // test-built binary
		cmd.Dir = repo
		var stdout, stderr bytes.Buffer
		cmd.Stdout, cmd.Stderr = &stdout, &stderr
		err := cmd.Run()
		cancel()
		name := strings.Join(path, " ")
		assert.Error(t, err, "mgit %s zzstray must fail", name)
		assert.Contains(t, stderr.String(), "zzstray",
			"mgit %s zzstray must say on stderr what it refused (stdout %q, stderr %q)", name, stdout.String(), stderr.String())
	}
}
