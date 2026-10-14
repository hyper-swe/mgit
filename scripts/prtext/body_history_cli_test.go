package main

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Exercise the real verdict-check CLI with a disposable GraphQL transport.
// The fake command only supplies API data; fetching, judging and exit status
// all run in the built product binary.
func TestCLI_DescriptionHistory(t *testing.T) {
	dir := t.TempDir()
	bin := filepath.Join(dir, "prtext")
	cmd := exec.Command("go", "build", "-o", bin, ".") //nolint:gosec // G204: fixed build command into a test-owned directory
	out, err := cmd.CombinedOutput()
	require.NoError(t, err, string(out))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "gh"), []byte("#!/bin/sh\ncat >/dev/null\ncat \"$PRTEXT_FIXTURE\"\n"), 0700)) //nolint:gosec // G306: executable test-only transport shim
	terms, names := filepath.Join(dir, "terms"), filepath.Join(dir, "names")
	require.NoError(t, os.WriteFile(terms, []byte(digest(testTerm)+"\n"), 0600))
	require.NoError(t, os.WriteFile(names, []byte(digest(normalize(testName))+"\n"), 0600))
	for _, tt := range []struct {
		name, editor, body string
		want               int
	}{
		{"corrected bot description", "Bot", "clean", 0},
		{"human edit on bot PR", "User", "clean", 1},
		{"current bot description", "Bot", testTerm, 1},
	} {
		t.Run(tt.name, func(t *testing.T) {
			fixture := filepath.Join(dir, "pr.json")
			history := testTerm
			if tt.body == testTerm {
				history = "clean"
			}
			raw := fmt.Sprintf(`{"data":{"repository":{"pullRequest":{"createdAt":"2026-10-13T11:00:00Z","lastEditedAt":"2026-10-13T12:00:00Z","title":"clean","body":%q,"author":{"__typename":"Bot","login":"dependabot"},"userContentEdits":{"nodes":[{"editedAt":"2026-10-13T11:00:00Z","editor":{"__typename":%q,"login":"dependabot"},"diff":%q},{"editedAt":"2026-10-13T12:00:00Z","editor":{"__typename":"User","login":"maintainer"},"diff":"clean"}]}}}}}`, tt.body, tt.editor, history)
			require.NoError(t, os.WriteFile(fixture, []byte(raw), 0600))
			cmd := exec.Command(bin, "-repo", "owner/repo", "-pr", "7", "-terms", terms, "-names", names) //nolint:gosec // G204: test-built CLI and test-owned inputs
			cmd.Env = append(os.Environ(), "PATH="+dir+string(os.PathListSeparator)+os.Getenv("PATH"), "PRTEXT_FIXTURE="+fixture)
			out, err := cmd.CombinedOutput()
			code := 0
			if err != nil {
				var exit *exec.ExitError
				require.ErrorAs(t, err, &exit, string(out))
				code = exit.ExitCode()
			}
			assert.Equal(t, tt.want, code, string(out))
			assert.NotContains(t, string(out), testTerm)
		})
	}
}
