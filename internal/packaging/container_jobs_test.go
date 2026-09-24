package packaging

import (
	"regexp"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// The ubuntu:20.04 container jobs start with no git, so their first step is
// an apt install that runs BEFORE actions/checkout — before the fetch guard's
// script is on disk. That step used to install the whole libkrun toolchain in
// one unretried apt run under a flat 5-minute bound, and a slow
// archive.ubuntu.com mirror timed it out on main (d1535db, "The action
// 'Install build prerequisites' has timed out after 5 minutes") with nothing
// saying what it was doing. Now the only pre-checkout apt is the git bootstrap,
// retried inline with a per-attempt bound and a line per attempt, and
// everything heavier comes after checkout through the guarded
// scripts/release/linux-build-prereqs.sh. Refs: MGIT-143, MGIT-229
func TestContainerJobs_BootstrapBeforeCheckoutAndInstallTheToolchainThroughTheGuard(t *testing.T) {
	jobs := []struct{ file, id string }{
		{".github/workflows/ci.yml", "libkrun-linux"},
		{".github/workflows/e2e.yml", "linux-sandboxd"},
		{".github/workflows/e2e.yml", "sandbox-live-linux-libkrun"},
	}
	aptInstall := regexp.MustCompile(`apt-get install[^\n]*(?:\\\n[^\n]*)*`)
	for _, j := range jobs {
		t.Run(j.id, func(t *testing.T) {
			job := jobBlock(t, readRepoFile(t, j.file), j.id)
			require.Contains(t, job, "ubuntu:20.04", "the subject is the ubuntu:20.04 container jobs")
			before, after, ok := strings.Cut(job, "uses: actions/checkout@")
			require.True(t, ok, "%s has no checkout", j.id)

			for _, m := range aptInstall.FindAllString(before, -1) {
				for _, heavy := range []string{"build-essential", "libclang", "patchelf", "flex"} {
					assert.NotContains(t, m, heavy,
						"%s installs %s before checkout, outside the guard; only the git bootstrap belongs there", j.id, heavy)
				}
			}
			assert.Contains(t, before, "for attempt in 1 2 3", "%s: the pre-checkout bootstrap must retry", j.id)
			assert.Contains(t, before, "timeout 180 ", "%s: each bootstrap attempt must be bounded", j.id)
			assert.Contains(t, before, "apt bootstrap: attempt $attempt/3 started", "%s: each attempt must say it started", j.id)
			assert.Contains(t, after, "scripts/release/linux-build-prereqs.sh",
				"%s must install its toolchain after checkout through the guarded prereqs script", j.id)
		})
	}
}
