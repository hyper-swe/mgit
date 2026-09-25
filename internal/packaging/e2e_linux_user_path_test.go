package packaging

import (
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
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
		`/tmp/*) where="under /tmp"`,                           // and the leg says which case it ran
	} {
		assert.Contains(t, script, want, "the user path must carry %q", want)
	}

	wf := readRepoFile(t, filepath.Join(".github", "workflows", "e2e.yml"))
	step := stepBlock(t, jobBlock(t, wf, "linux-user-path"), "- name: The user path")
	assert.Contains(t, step, `root="$RUNNER_TEMP/scratch"`, "one leg puts the scratch root outside /tmp")
	assert.Contains(t, step, `[ "${{ matrix.install }}" = install-script ]`, "only the install-script leg; the archive leg keeps /tmp")
	assert.Contains(t, step, `bash scripts/e2e/linux_user_path.sh "$BIN" ${root:+"$root"}`, "and the script receives it")
}
