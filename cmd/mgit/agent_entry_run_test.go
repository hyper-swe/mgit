package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/hyper-swe/mgit/internal/docs"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// splitCommand splits a documented command line the way a shell would for
// the simple forms MGIT.md uses: words, and double-quoted words.
func splitCommand(line string) []string {
	var out []string
	var cur strings.Builder
	inQuote, have := false, false
	for _, r := range line {
		switch {
		case r == '"':
			inQuote, have = !inQuote, true
		case r == ' ' && !inQuote:
			if have {
				out = append(out, cur.String())
				cur.Reset()
				have = false
			}
		default:
			cur.WriteRune(r)
			have = true
		}
	}
	if have {
		out = append(out, cur.String())
	}
	return out
}

// firstBashBlock returns the lines of the first fenced bash block.
func firstBashBlock(t *testing.T, doc string) []string {
	t.Helper()
	_, rest, ok := strings.Cut(doc, "```bash\n")
	require.True(t, ok, "MGIT.md has a bash block")
	block, _, ok := strings.Cut(rest, "```")
	require.True(t, ok)
	return strings.Split(strings.TrimSpace(block), "\n")
}

// The documented workflow is RUN, in order, on a fresh repository that has
// just been initialized: not matched by substring. An agent that follows
// MGIT.md after `mgit init` must reach a patch that applies in a plain git
// checkout. Refs: HSWE-19.3
func TestTheDocumentedWorkflowRunsOnAFreshInit(t *testing.T) {
	src := t.TempDir()
	git := func(dir string, args ...string) {
		t.Helper()
		cmd := exec.Command("git", append([]string{"-C", dir}, args...)...) //nolint:gosec // test-controlled arguments
		out, err := cmd.CombinedOutput()
		require.NoError(t, err, "git %v: %s", args, out)
	}
	git(src, "init", "-q", "-b", "main")
	git(src, "config", "user.email", "t@example.com")
	git(src, "config", "user.name", "T")
	require.NoError(t, os.WriteFile(filepath.Join(src, "a.txt"), []byte("a\n"), 0o600))
	git(src, "add", "a.txt")
	git(src, "commit", "-q", "-m", "base")
	t.Chdir(src)
	require.NoError(t, runCLI(t, "init"))

	steps := firstBashBlock(t, docs.AgentInstructions())
	ran := 0
	for _, line := range steps {
		switch {
		case strings.HasPrefix(line, "#"):
			require.NoError(t, os.WriteFile(filepath.Join(src, "change.txt"), []byte("change\n"), 0o600), "the edit step")
		case strings.HasPrefix(line, "mgit "):
			args := splitCommand(line)[1:]
			require.NoError(t, runCLI(t, args...), "documented command failed: %s", line)
			ran++
		default:
			t.Fatalf("an unexpected line in the documented block: %q", line)
		}
	}
	require.GreaterOrEqual(t, ran, 4, "the workflow ran its commands")

	// the patch the workflow writes holds the task's change only, and applies
	patch, err := os.ReadFile(filepath.Join(src, "PROJ-1.patch")) //nolint:gosec // test fixture path
	require.NoError(t, err)
	assert.Contains(t, string(patch), "change.txt")
	assert.NotContains(t, string(patch), "a.txt", "the baseline commit keeps pre-existing files out of the task patch")
	assert.NotContains(t, string(patch), "MGIT.md", "the files init wrote ride in the baseline, not the task")

	tgt := t.TempDir()
	git(tgt, "init", "-q", "-b", "main")
	git(tgt, "apply", filepath.Join(src, "PROJ-1.patch"))
	_, statErr := os.Stat(filepath.Join(tgt, "change.txt"))
	assert.NoError(t, statErr, "git apply of the documented patch creates the file")
}

// The text points at nothing init does not write, and names no flag that
// the commands do not take.
func TestAgentInstructionsHaveNoDeadReferences(t *testing.T) {
	doc := docs.AgentInstructions()
	for _, dead := range []string{"MCP_TOOLS.md", "CLI_REFERENCE.md", "SKILL.md", "--message=", "mtix"} {
		assert.NotContains(t, doc, dead)
	}
}
