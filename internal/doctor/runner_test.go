package doctor

import (
	"context"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// recordingCheck notes that it ran and returns a canned verdict.
type recordingCheck struct {
	name   string
	result Result
	ran    *[]string
	panics bool
}

func (c recordingCheck) Name() string { return c.name }

func (c recordingCheck) Run(context.Context) Result {
	*c.ran = append(*c.ran, c.name)
	if c.panics {
		panic("a check exploded")
	}
	r := c.result
	r.Name = c.name
	return r
}

// Run is the entry point every caller uses and the only part of the framework
// with no coverage at all. Its contract is small and load-bearing: every check
// runs, in order, and exactly one Result comes back per check — a runner that
// silently skipped one would produce a report whose absences read as passes,
// which is the whole thing this package exists to prevent. Refs: MGIT-162
func TestRun_ExecutesEveryCheckInOrder_AndReturnsOneResultEach(t *testing.T) {
	var ran []string
	checks := []Check{
		recordingCheck{name: "first", result: Result{Status: StatusOK}, ran: &ran},
		recordingCheck{name: "second", result: Result{Status: StatusFailed}, ran: &ran},
		recordingCheck{name: "third", result: Result{Status: StatusNotChecked}, ran: &ran},
	}

	got := Run(context.Background(), checks)

	assert.Equal(t, []string{"first", "second", "third"}, ran,
		"every check must run, and in the order given")
	require.Len(t, got, len(checks), "one result per check, never fewer")
	assert.Equal(t, []string{"first", "second", "third"},
		[]string{got[0].Name, got[1].Name, got[2].Name},
		"results must stay aligned with the checks that produced them")
	assert.Equal(t, []Status{StatusOK, StatusFailed, StatusNotChecked},
		[]Status{got[0].Status, got[1].Status, got[2].Status})
}

// An empty check list is a report with nothing in it, not a crash and not a
// nil slice a caller has to special-case.
func TestRun_NoChecks_IsAnEmptyReportNotAFailure(t *testing.T) {
	got := Run(context.Background(), nil)

	require.NotNil(t, got, "callers range over this; nil invites a different bug")
	assert.Empty(t, got)
	assert.False(t, Failed(got), "nothing checked is not a failure")
	assert.Contains(t, Render(got), "absence of evidence")
}

// A check must be handed the caller's context. doctor is run behind a CLI
// whose context carries cancellation, and a probe that ignored it would hang a
// diagnostic — the one command a stuck operator reaches for.
func TestRun_PassesTheCallersContextThrough(t *testing.T) {
	type key struct{}
	ctx := context.WithValue(context.Background(), key{}, "carried")
	var seen any

	Run(ctx, []Check{ctxCheck{fn: func(c context.Context) { seen = c.Value(key{}) }}})

	assert.Equal(t, "carried", seen, "a check that cannot see the context cannot be canceled")
}

type ctxCheck struct{ fn func(context.Context) }

func (ctxCheck) Name() string { return "ctx" }
func (c ctxCheck) Run(ctx context.Context) Result {
	c.fn(ctx)
	return Result{Name: "ctx", Status: StatusOK}
}

// EVERY REGISTERED CHECK NAMES A REAL INCIDENT, and the case list is read from
// the SOURCE rather than from the slice the package exports.
//
// The rule this enforces (R-H300 rule 5) is that a check outlives the people
// who remember why it exists. A test that iterated a checks slice would prove
// only that the slice agrees with itself: add a check and forget the incident,
// and the test still passes because the new check is not in the list either.
//
// So the list comes from checks.go — every type declaring `Run(...) Result` —
// and each must appear with an incident. A new check that forgets its ticket
// fails here without anyone updating a test. Refs: MGIT-162, R-H300
func TestEveryCheckInTheSource_NamesTheIncidentItConverts(t *testing.T) {
	src := readSource(t, "checks.go")

	// Types with a Run method returning Result are the checks.
	declared := regexp.MustCompile(`func \((?:\w+ )?(\w+)\) Run\([^)]*\) Result`).FindAllStringSubmatch(src, -1)
	require.NotEmpty(t, declared, "the source scan found no checks; the pattern has drifted")

	names := make([]string, 0, len(declared))
	for _, m := range declared {
		names = append(names, m[1])
	}
	sort.Strings(names)
	t.Logf("checks found in checks.go: %v", names)

	for _, typeName := range names {
		t.Run(typeName, func(t *testing.T) {
			body := methodBody(t, src, typeName)
			assert.Regexp(t, `Incident:\s*"MGIT-\d+"`, body,
				"%s must set Incident to the ticket it converts, so the reason it "+
					"exists is discoverable from its own output", typeName)
			assert.Contains(t, src, "Refs: MGIT-162",
				"the package's checks trace back to the rule that mandates them")
		})
	}
}

// A FAILED verdict must always carry a remedy — again read from the source, so
// a check added without one is caught even though nothing lists it.
//
// "localhost does not resolve" and stop has moved the mystery, not removed it.
// Refs: MGIT-162, R-H300
func TestEveryFailedVerdictInTheSource_SetsARemedy(t *testing.T) {
	src := readSource(t, "checks.go")
	for _, block := range strings.Split(src, "// Run implements Check.")[1:] {
		if !strings.Contains(block, "StatusFailed") {
			continue
		}
		assert.Contains(t, block, "r.Remedy =",
			"a check that can report StatusFailed must also set a Remedy:\n%s",
			firstLines(block, 6))
	}
}

// Failed() is the exit code. Its one rule — not-checked never fails it — is
// asserted across every combination rather than by example, because the rule
// is about the SET and a two-element example cannot express it.
// Refs: MGIT-162
func TestFailed_OnlyAFailedVerdictFailsTheExitCode(t *testing.T) {
	all := []Status{StatusOK, StatusNotChecked, StatusFailed}
	for _, a := range all {
		for _, b := range all {
			name := string(a) + "+" + string(b)
			t.Run(name, func(t *testing.T) {
				got := Failed([]Result{{Status: a}, {Status: b}})
				want := a == StatusFailed || b == StatusFailed
				assert.Equal(t, want, got,
					"only StatusFailed may fail the exit code; a check that could not RUN "+
						"is an absence of evidence, and treating it as a failure trains "+
						"readers to ignore the code")
			})
		}
	}
}

// An unknown status must not be silently treated as a pass. A verdict the
// framework does not recognize is rendered distinctly from ok, so a future
// third state cannot inherit ok's glyph by default.
func TestRender_AnUnrecognizedStatus_IsNotRenderedAsAPass(t *testing.T) {
	out := Render([]Result{{Name: "x", Status: Status("something-new"), Summary: "s", Incident: "MGIT-1"}})

	assert.NotContains(t, out, "ok    x",
		"an unrecognized verdict must never wear the ok marker")
	assert.Contains(t, out, "?", "it falls back to the cannot-tell glyph")
}

// The report carries, for every result, the three things a reader acts on:
// the verdict, the check's name, and the incident behind it. Asserted as
// presence per result rather than by matching the whole rendered block, so
// changing the column widths does not fail this. Refs: MGIT-162
func TestRender_EveryResultCarriesItsVerdictNameAndIncident(t *testing.T) {
	results := []Result{
		{Name: "tree/nested-git", Status: StatusOK, Summary: "clean", Incident: "MGIT-157"},
		{Name: "guest/localhost", Status: StatusFailed, Summary: "broken",
			Remedy: "rebuild the base", Incident: "MGIT-159"},
		{Name: "some/other", Status: StatusNotChecked, Summary: "could not ask",
			Reason: "no daemon", Incident: "MGIT-999"},
	}

	out := Render(results)

	for _, r := range results {
		assert.Contains(t, out, r.Name)
		assert.Contains(t, out, r.Summary)
		assert.Contains(t, out, "from: "+r.Incident,
			"the incident must reach the reader, not just the struct")
	}
	assert.Contains(t, out, "remedy: rebuild the base")
	assert.Contains(t, out, "why not: no daemon")
	assert.NotContains(t, out, "absence of evidence",
		"the reassurance line belongs only on a report with no failures")
}

// A remedy on a NON-failed result is not printed: an ok line carrying advice
// reads as a problem, and a not-checked line carrying advice reads as a
// diagnosis nobody made.
func TestRender_RemedyIsPrintedOnlyForAFailure(t *testing.T) {
	out := Render([]Result{
		{Name: "a", Status: StatusOK, Remedy: "do not show me", Incident: "MGIT-1"},
		{Name: "b", Status: StatusNotChecked, Remedy: "nor me", Reason: "why", Incident: "MGIT-2"},
	})

	assert.NotContains(t, out, "do not show me")
	assert.NotContains(t, out, "nor me")
	assert.Contains(t, out, "why not: why")
}

// A failure with no remedy still renders — the check is wrong, but the reader
// must not lose the finding to a formatting branch.
func TestRender_AFailureWithNoRemedy_StillReportsTheFinding(t *testing.T) {
	out := Render([]Result{{Name: "a", Status: StatusFailed, Summary: "it is broken", Incident: "MGIT-1"}})

	assert.Contains(t, out, "FAIL")
	assert.Contains(t, out, "it is broken")
	assert.NotContains(t, out, "remedy:")
}

// A long summary is not truncated. The format string pads a name column; a
// summary wider than it must still arrive whole, since the summary is the
// finding.
func TestRender_ALongSummaryIsNotTruncated(t *testing.T) {
	long := strings.Repeat("consequence ", 40)
	out := Render([]Result{{Name: "n", Status: StatusFailed, Summary: long, Incident: "MGIT-1"}})

	assert.Contains(t, out, long)
}

// readSource reads a file from this package's own directory.
func readSource(t *testing.T, name string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Clean(name))
	require.NoError(t, err, "the source scan must read %s; a rename breaks this test loudly", name)
	return string(b)
}

// methodBody returns the text of typeName's Run method.
func methodBody(t *testing.T, src, typeName string) string {
	t.Helper()
	re := regexp.MustCompile(`func \((?:\w+ )?` + typeName + `\) Run\([^)]*\) Result \{`)
	loc := re.FindStringIndex(src)
	require.NotNil(t, loc, "could not locate %s.Run", typeName)
	rest := src[loc[1]:]
	if end := strings.Index(rest, "\n}\n"); end >= 0 {
		return rest[:end]
	}
	return rest
}

func firstLines(s string, n int) string {
	lines := strings.SplitN(s, "\n", n+1)
	if len(lines) > n {
		lines = lines[:n]
	}
	return strings.Join(lines, "\n")
}
