package packaging

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// THE LINUX USER PATH IS PROVEN WITHOUT TEST HOOKS. mgit's firecracker live
// gate boots its guests through MGIT_GUEST_KERNEL / MGIT_GUEST_ROOTFS, which
// a release user never has, so a green there said nothing about the path a
// user walks. That is how hyper-swe/mgit#12 stayed open for six weeks beside
// a green Linux column. This leg runs the release-shaped layout through the
// documented steps with no hook set. A hook added to the job, or the script's
// refusal removed, would let it go green without the user path working.
// Refs: MGIT-229
func TestE2E_TheLinuxUserPathRunsWithoutTestHooks(t *testing.T) {
	wf := readRepoFile(t, filepath.Join(".github", "workflows", "e2e.yml"))
	job := jobBlock(t, wf, "linux-user-path")
	for _, want := range []string{
		"scripts/e2e/linux_user_path.sh",
		// the archive's own build ids, so the layout is the one users receive
		"--id mgit --id mgit-sandboxd-linux --id mgit-guest",
		`"$bin/guest/mgit-guest"`,
	} {
		assert.Contains(t, job, want, "the linux-user-path job must carry %q", want)
	}
	for _, hook := range []string{"MGIT_GUEST_KERNEL", "MGIT_GUEST_ROOTFS", "MGIT_GUEST_BASE", "MGIT_GUEST_IMAGE"} {
		assert.NotContains(t, job, hook, "the user path must not be booted through the test hook %s", hook)
	}

	script := readRepoFile(t, filepath.Join("scripts", "e2e", "linux_user_path.sh"))
	assert.Contains(t, script, "for hook in MGIT_GUEST_KERNEL MGIT_GUEST_ROOTFS MGIT_GUEST_BASE MGIT_GUEST_IMAGE; do",
		"the script refuses to run with any test hook set")
	assert.Contains(t, script, "LINUX USER PATH: PASS", "the verdict line the job and a reader rely on")
	for _, verb := range []string{"sandbox base from", "sandbox launch", "mgit run --", "sandbox sync", "sandbox export", "sandbox remove",
		// the loop's per-round deletion canary (MGIT-230.2)
		"sandbox sync --task-id \"$TASK\" --dry-run", "[ -e canary-a.txt ] && echo present || echo gone",
		// the loop's exec contract (MGIT-230.3)
		"nohup sleep 120", "kill -0", "exit 7", "/proc/meminfo", "dmesg", "name=\"$(gx 'id -un')\""} {
		assert.Contains(t, script, verb, "the user path includes %q", verb)
	}
}

// THE WORKTREE IS SEEN AT ITS HOST PATH, UNDER /tmp AND OUTSIDE IT. mgit
// mounts the worktree in the guest at its identical host path, and `mgit
// run` runs in the guest at the caller's canonical cwd. The leg's worktree
// sat under /tmp only because the runner sets no TMPDIR, which nothing
// printed, and nothing compared the guest's working directory with the host
// path. A worktree outside /tmp needs its mount point made by shadowing a
// directory the base image ships (MGIT-230.7), which no user-path run had
// exercised. So the script names the worktree's physical host path and
// whether it is under /tmp, and fails unless the guest's pwd is exactly that
// path. One leg keeps the scratch root in /tmp, and the other passes one
// outside it. Refs: MGIT-230.3, MGIT-230.7
func TestE2E_TheLinuxUserPathSeesTheWorktreeAtItsHostPath_UnderAndOutsideTmp(t *testing.T) {
	script := readRepoFile(t, filepath.Join("scripts", "e2e", "linux_user_path.sh"))
	for _, want := range []string{
		`ROOT="${2:-${TMPDIR:-/tmp}}"`,                         // a scratch root the caller may choose
		`PH="$(cd "$P" && pwd -P)"`,                            // the worktree's physical host path
		`gwd="$(cd "$P" && timeout 120 mgit run -- pwd 2>&1)"`, // the guest's working directory
		`[ "$gwd" = "$PH" ] || fail "exec"`,                    // must be exactly that path
		`where="$(where_is "$PH" /tmp)"`,                       // and the leg says which case it ran
	} {
		assert.Contains(t, script, want, "the user path must carry %q", want)
	}

	wf := readRepoFile(t, filepath.Join(".github", "workflows", "e2e.yml"))
	step := stepBlock(t, jobBlock(t, wf, "linux-user-path"), "- name: The user path")
	assert.Contains(t, step, `root="$RUNNER_TEMP/scratch"`, "one leg puts the scratch root outside /tmp")
	assert.Contains(t, step, `[ "${{ matrix.install }}" = install-script ]`, "only the install-script leg; the archive leg keeps /tmp")
	assert.Contains(t, step, `bash scripts/e2e/linux_user_path.sh "$BIN" ${root:+"$root"}`, "and the script receives it")
}

// THE /tmp LABEL COMPARES PHYSICAL PATHS. The script prints whether the
// worktree is under /tmp or outside it, and that line is the only record of
// which case a leg ran. It matched the worktree's physical path against a
// literal "/tmp/*", so wherever /tmp is a symlink (macOS: /tmp →
// /private/tmp) a worktree under /tmp was labeled "outside /tmp". The label
// now comes from where_is, which resolves the root as it resolves the path.
// This runs the script's own where_is against a real directory and a
// symlink to it, independent of the host's /tmp. Refs: MGIT-266
func TestLinuxUserPath_TheTmpLabelComparesPhysicalPaths(t *testing.T) {
	script := readRepoFile(t, filepath.Join("scripts", "e2e", "linux_user_path.sh"))
	var fn string
	for _, line := range strings.Split(script, "\n") {
		if strings.HasPrefix(line, "where_is() {") {
			fn = line
		}
	}
	require.NotEmpty(t, fn, "the script defines where_is PATH ROOT on one line")

	dir := t.TempDir()
	real := filepath.Join(dir, "real")
	require.NoError(t, os.MkdirAll(filepath.Join(real, "wt"), 0o750))
	link := filepath.Join(dir, "link")
	require.NoError(t, os.Symlink(real, link))
	physical, err := filepath.EvalSymlinks(filepath.Join(real, "wt"))
	require.NoError(t, err)
	outside := t.TempDir()

	for _, tt := range []struct{ path, root, want string }{
		{physical, link, "under /tmp"},  // the root is a symlink to the path's parent
		{physical, real, "under /tmp"},  // the root is already physical
		{outside, link, "outside /tmp"}, // elsewhere
	} {
		out, err := exec.Command("bash", "-c", fn+"\nwhere_is \"$1\" \"$2\"", "bash", tt.path, tt.root).CombinedOutput() //nolint:gosec // G204: the script's own function, test-only arguments
		require.NoError(t, err, "%s", out)
		assert.Equal(t, tt.want, strings.TrimSpace(string(out)), "where_is %s %s", tt.path, tt.root)
	}
}

// THE CANARY'S DROP RECORD NAMES THE IDENTITY IT RAN AS. Step 7b runs
// `sync; echo 2 > /proc/sys/vm/drop_caches` through `mgit run` and prints
// whether the drop took effect. `mgit run` runs as the daemon's identity,
// the agent's (MGIT-151). The settle step's own execs do not use that
// identity (MGIT-270), so a line that called this "the settle's cache drop"
// read as an instrument of the settle that it is not. It now names the
// identity it measured, and no line calls it the settle's. The command is
// unchanged. Refs: MGIT-270, MGIT-230.2
func TestE2E_TheCanaryDropRecordNamesTheIdentityItRanAs(t *testing.T) {
	script := readRepoFile(t, filepath.Join("scripts", "e2e", "linux_user_path.sh"))
	for _, want := range []string{
		`mgit run -- sh -c 'sync; echo 2 > /proc/sys/vm/drop_caches && echo took-effect || echo did-not-take-effect'`,
		`echo "  a cache drop as the agent's identity (mgit run; not the settle step's identity): $drop"`,
	} {
		assert.Contains(t, script, want, "the record names the identity it ran as: %q", want)
	}
	assert.False(t, strings.Contains(script, "the settle's cache drop"),
		"no line labels the mgit run measurement as the settle's cache drop")
}
