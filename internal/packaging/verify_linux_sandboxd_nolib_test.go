package packaging

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Found by the MGIT-230.6 smoke's first run against the published v0.6.8,
// whose Linux daemon ships with no bundled libraries: the verifier exited 2
// and printed NOTHING. Under `set -euo pipefail`, `ls` of a missing library
// failed the command substitution and killed the script at the assignment,
// one line before the fail() that names the problem. A check that goes red in
// silence reads as a crash, not a verdict. Refs: MGIT-230.6, MGIT-229
func TestVerifyLinuxSandboxd_ADaemonWithNoBundledLibraries_IsNamedNotSilent(t *testing.T) {
	dir := t.TempDir()
	//nolint:gosec // G306: a stand-in daemon must be executable
	require.NoError(t, os.WriteFile(filepath.Join(dir, "mgit-sandboxd"), []byte("#!/bin/sh\necho x\n"), 0o750))
	//nolint:gosec,noctx // G204: the repository's own verifier against a test fixture
	out, err := exec.Command("bash", filepath.Join(repoRoot(t), "scripts", "release", "verify-linux-sandboxd.sh"), dir).CombinedOutput()
	require.Error(t, err, "a daemon with no libkrun beside it must fail the verifier")
	var exitErr *exec.ExitError
	require.ErrorAs(t, err, &exitErr)
	assert.Equal(t, 1, exitErr.ExitCode(), "a verdict (fail), not a crash of the script")
	assert.Contains(t, string(out), "verify-linux-sandboxd: FAIL: no libkrun.so.* in", "and it says what is missing:\n%s", out)
}
