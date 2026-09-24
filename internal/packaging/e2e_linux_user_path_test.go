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
		"sandbox sync --task-id \"$TASK\" --dry-run", "[ -e canary-a.txt ] && echo present || echo gone"} {
		assert.Contains(t, script, verb, "the user path includes %q", verb)
	}
}
