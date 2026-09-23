package packaging

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// A LATE PASS LOOKS EXACTLY LIKE A PROMPT ONE. This workflow's scheduled run
// has been created about five hours after its cron every day since
// 2026-08-27 — 25 days, measured — and nobody noticed because it stays green:
// the run is green, the list is green, and a reader between the cron and the
// run is reading YESTERDAY's result believing it is today's. The scheduled
// run therefore publishes its result WITH its age on the commit it tested,
// and the age is measured against the cron READ FROM THIS FILE, never a
// second copy that could drift from the schedule it claims to measure.
// Refs: MGIT-220, MGIT-174
func TestE2E_AScheduledRunPublishesItsResultWithItsAge(t *testing.T) {
	wf := readRepoFile(t, filepath.Join(".github", "workflows", "e2e.yml"))
	require.Contains(t, wf, `cron: "0 3 * * *"`, "the schedule this job measures against")

	job := jobBlock(t, wf, "schedule-age")
	for _, want := range []string{
		"if: ${{ always() && github.event_name == 'schedule' }}",
		"statuses: write",
		"actions/checkout@v4",
		// the cron is read from the file, so there is nothing to keep in step
		".github/workflows/e2e.yml",
		"grep -o 'cron:",
		"-f context=e2e-schedule-age",
		// both halves in the one description a reader sees on the commit
		"needs.posture.result",
		"needs.install-channels.result",
		"needs.sandbox-live-linux.result",
		"needs.sandbox-live-linux-libkrun.result",
	} {
		assert.Contains(t, job, want, "the schedule-age job must carry %q", want)
	}
	assert.Regexp(t, `needs:.*posture.*install-channels.*sandbox-live-linux.*sandbox-live-linux-libkrun`, job,
		"it reports on every job of the run, so a green that hides a failure cannot be published as one")
	assert.NotRegexp(t, `due=.*"today 0?3:00"`, job,
		"the hour must come from the cron line, not be restated: a second copy drifts from the schedule")
	// "0 3 * * *" reads 03:00Z, not 3:0Z. A time the reader has to decode is
	// a time they will misread, and this status exists to be read at a glance.
	assert.Contains(t, job, "%02d", "the cron's fields are zero-padded for the reader")
	assert.Contains(t, job, "${hh}:${mm}Z", "and the padded pair is what the description prints")
}

// jobBlock returns one job's YAML block, from its key to the next job at the
// same indentation. Reading the whole file would let an assertion pass on a
// line that lives in a different job.
func jobBlock(t *testing.T, wf, id string) string {
	t.Helper()
	start := strings.Index(wf, "\n  "+id+":\n")
	require.GreaterOrEqual(t, start, 0, "no job %q in the workflow", id)
	rest := wf[start+1:]
	for i := 1; ; i++ {
		next := strings.Index(rest[i:], "\n  ")
		if next < 0 {
			return rest
		}
		i += next
		// a sibling job starts at two spaces and a name, not deeper
		if len(rest) > i+3 && rest[i+3] != ' ' && rest[i+3] != '#' && rest[i+1] == ' ' {
			return rest[:i]
		}
	}
}
