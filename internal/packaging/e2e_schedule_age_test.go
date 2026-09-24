package packaging

import (
	"io/fs"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// A LATE PASS LOOKS EXACTLY LIKE A PROMPT ONE. e2e's scheduled run has been
// created about five hours after its cron every day since 2026-08-27 (25 days,
// measured), and nobody noticed because it stays green: the run is green, the
// list is green, and a reader between the cron and the run is reading
// YESTERDAY's result believing it is today's. The scheduled run's result is
// therefore published WITH its timing on the commit it tested, and the timing
// is measured against the cron READ FROM e2e.yml, never a second copy that
// could drift from the schedule it claims to measure.
//
// The publisher is its own workflow, run when e2e completes. As a job of
// e2e.yml it stopped release.yml from starting at all, because the release
// calls e2e.yml and could not grant it statuses:write (MGIT-244). The move is
// where a port goes wrong silently, so this pins the run it reads: ITS start,
// ITS conclusion and ITS commit, never this workflow's own.
// Refs: MGIT-220, MGIT-174, MGIT-244
func TestE2E_AScheduledRunPublishesItsResultWithItsAge(t *testing.T) {
	e2e := readRepoFile(t, filepath.Join(".github", "workflows", "e2e.yml"))
	require.Contains(t, e2e, `cron: "0 3 * * *"`, "the schedule this job measures against")
	require.Contains(t, e2e, "\nname: e2e\n", "the workflow name the publisher's trigger names")

	const file = "e2e-schedule-age.yml"
	wf := readRepoFile(t, filepath.Join(".github", "workflows", file))
	for _, want := range []string{"workflow_run:", "workflows: [e2e]", "types: [completed]"} {
		assert.Contains(t, wf, want, "the publisher runs when an e2e run completes: %q", want)
	}
	perms := parseWorkflowPerms(t, file, wf)
	// statuses:write alone cannot read the runs API
	assert.Equal(t, map[string]string{"statuses": "write", "actions": "read"}, perms.top)

	job := jobBlock(t, wf, "schedule-age")
	for _, want := range []string{
		"if: ${{ github.event.workflow_run.event == 'schedule' }}",
		"actions/checkout@", // it reads the workflow file, so it checks the repository out (pinned: MGIT-246)
		// the cron is read from the file, so there is nothing to keep in step
		".github/workflows/e2e.yml",
		"grep -o 'cron:",
		"-f context=e2e-schedule-age",
		// the COMPLETED RUN's fields: its conclusion covers every job of it,
		// including jobs added to e2e.yml after this was written
		"CONCLUSION: ${{ github.event.workflow_run.conclusion }}",
		`[ "$CONCLUSION" != success ]`,
		"RUN_ID: ${{ github.event.workflow_run.id }}",
		`actions/runs/$RUN_ID"`,
		"RUN_SHA: ${{ github.event.workflow_run.head_sha }}",
		`statuses/$RUN_SHA"`,
	} {
		assert.Contains(t, job, want, "the schedule-age job must carry %q", want)
	}
	// This workflow's OWN run starts after e2e ends, and its own commit is
	// the default branch's head, not the one e2e tested. Either would publish
	// a wrong fact that reads right.
	assert.NotContains(t, job, "$GITHUB_RUN_ID", "the start measured must be the scheduled run's, not this one's")
	assert.NotContains(t, job, "$GITHUB_SHA", "the status belongs on the commit the scheduled run tested")
	// A write token runs this: the run's fields reach the script as data.
	at := strings.Index(job, "run: |")
	require.GreaterOrEqual(t, at, 0, "the publisher's script is a `run: |` block")
	script := job[at:]
	assert.NotContains(t, script, "${{", "no expression is spliced into the script this write token runs")

	assert.NotRegexp(t, `due=.*"today 0?3:00"`, job,
		"the hour must come from the cron line, not be restated: a second copy drifts from the schedule")
	// THE PUBLISHER RUNS AFTER THE RUN ENDS, so its own clock is later than
	// the run's end: "started" must come from the run's own metadata, or the
	// published delay is the scheduling lateness plus however long the run
	// took (median 0.20h here, max 6.00h). And the times are absolute,
	// because an age baked at publication is wrong for every reader after it.
	for _, want := range []string{
		// the CALL SITE, not the word: `run_started_at` also appears in the
		// permission comment, and a pin a comment satisfies pins nothing.
		// With the API read deleted, an earlier form of this test still passed.
		"--jq .run_started_at",
		// gh writes an ERROR BODY to stdout and skips --jq when the request
		// fails, so a non-empty capture is not a value: it is taken only if
		// gh exited 0 AND `date` accepts it as an instant.
		`started_epoch=$(date -u -d "$raw" +%s 2>/dev/null)`,
		"published",                        // the second, distinct instant
		`[ "$state" = success ] || exit 1`, // the job's conclusion agrees with its own text
		"state=error",                      // a timing it could not read is not a pass
		"for attempt",                      // …but a transient API blip is retried before it reds a green run
	} {
		assert.Contains(t, job, want, "the schedule-age job must carry %q", want)
	}
	assert.NotContains(t, job, `--jq .run_started_at 2>/dev/null || true`,
		"the bare capture stores gh's error body as the timestamp")
	assert.NotContains(t, job, "this result is",
		"an age baked at publication decays: print absolute instants and let the reader subtract")

	// "0 3 * * *" reads 03:00Z, not 3:0Z. A time the reader has to decode is
	// a time they will misread, and this status exists to be read at a glance.
	assert.Contains(t, job, "%02d", "the cron's fields are zero-padded for the reader")
	assert.Contains(t, job, "${hh}:${mm}Z", "and the padded pair is what the description prints")
}

// "NIGHTLY" WAS A CLAIM ABOUT TIME, AND IT WAS FALSE. The scheduled run is
// created hours after its 03:00Z cron — +5.3h on the first run that said so
// (the e2e-schedule-age status) — so it lands mid-morning UTC and in working
// hours further east, and "the nightly" told a reader it had happened while
// they slept. Every place the gate is read calls it what it is: the daily
// scheduled run. The files are WALKED, not listed, so a new document cannot
// bring the word back unnoticed; the source of the case list is the tree, not
// this test. Refs: MGIT-220
func TestE2E_TheScheduledRunIsNotCalledNightly(t *testing.T) {
	root := repoRoot(t)
	var hits []string
	for _, dir := range []string{".github", "docs", "scripts"} {
		err := filepath.WalkDir(filepath.Join(root, dir), func(p string, d fs.DirEntry, err error) error {
			if err != nil || d.IsDir() {
				return err
			}
			hits = append(hits, nightlyLines(t, root, p)...)
			return nil
		})
		require.NoError(t, err, "walk %s", dir)
	}
	top, err := os.ReadDir(root)
	require.NoError(t, err)
	for _, e := range top {
		if !e.IsDir() && (e.Name() == "Makefile" || strings.HasSuffix(e.Name(), ".md")) {
			hits = append(hits, nightlyLines(t, root, filepath.Join(root, e.Name()))...)
		}
	}
	assert.Empty(t, hits, "the scheduled e2e run is daily and hours late, not nightly — say what it is")
}

// nightlyLines returns "path:line" for every line of one file that calls
// something "nightly", case-insensitively.
func nightlyLines(t *testing.T, root, p string) []string {
	t.Helper()
	//nolint:gosec // G304: test-only; p comes from walking the module's own tree
	b, err := os.ReadFile(p)
	require.NoError(t, err)
	rel, err := filepath.Rel(root, p)
	require.NoError(t, err)
	var out []string
	for i, line := range strings.Split(string(b), "\n") {
		if strings.Contains(strings.ToLower(line), "nightly") {
			out = append(out, rel+":"+strconv.Itoa(i+1))
		}
	}
	return out
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
