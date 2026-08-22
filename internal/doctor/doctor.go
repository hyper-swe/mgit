// Package doctor runs cheap standing checks for conditions that have already
// cost someone a diagnosis.
//
// It exists because of R-H300 rule 5: an incident is not closed until the
// thing that went wrong becomes a thing the tool asks itself, the way a bug is
// not closed until it has a regression test. Every check names the incident it
// came from, so the reason it exists outlives the people who remember it.
//
// Refs: MGIT-162, R-H300
package doctor

import (
	"context"
	"fmt"
	"strings"
)

// Status is a check's verdict.
type Status string

const (
	// StatusOK means the check ran and the condition holds.
	StatusOK Status = "ok"
	// StatusFailed means the check ran and found the condition it exists to catch.
	StatusFailed Status = "failed"
	// StatusNotChecked means the check could NOT run, and says why.
	//
	// A first-class verdict rather than an absence, because a diagnostic that
	// silently skips is worse than no diagnostic: silence reads as a pass, and
	// a reader who believes a check passed has been misled by the very
	// instrument they consulted to avoid being misled. Refs: MGIT-162, R-H300
	StatusNotChecked Status = "not-checked"
)

// Result is one check's outcome.
type Result struct {
	Name   string `json:"name"`
	Status Status `json:"status"`
	// Summary states what was found, in the reader's terms.
	Summary string `json:"summary"`
	// Remedy is what to DO about a failure. A check that reports a problem and
	// stops has moved the mystery rather than removed it.
	Remedy string `json:"remedy,omitempty"`
	// Reason explains why a check could not run (not-checked only).
	Reason string `json:"reason,omitempty"`
	// Incident is the ticket whose failure this check converts, so the reason
	// the check exists is discoverable from its own output. Refs: R-H300
	Incident string `json:"incident"`
}

// Check is one standing condition.
type Check interface {
	Name() string
	// Run performs the check. It returns a Result rather than an error: a
	// check that cannot run reports not-checked WITH a reason, which is
	// information rather than failure.
	Run(ctx context.Context) Result
}

// Run executes every check in order.
func Run(ctx context.Context, checks []Check) []Result {
	out := make([]Result, 0, len(checks))
	for _, c := range checks {
		out = append(out, c.Run(ctx))
	}
	return out
}

// Failed reports whether any check found the condition it exists to catch.
//
// A check that could not RUN is deliberately not a failure: it is an absence
// of evidence, and treating it as one would train readers to ignore the exit
// code — which is how a gate stops being consulted at all. Refs: MGIT-162
func Failed(results []Result) bool {
	for _, r := range results {
		if r.Status == StatusFailed {
			return true
		}
	}
	return false
}

// Render writes the human report.
func Render(results []Result) string {
	var b strings.Builder
	for _, r := range results {
		fmt.Fprintf(&b, "%-5s %-26s %s\n", marker(r.Status), r.Name, r.Summary)
		if r.Status == StatusFailed && r.Remedy != "" {
			fmt.Fprintf(&b, "      %-26s remedy: %s\n", "", r.Remedy)
		}
		if r.Status == StatusNotChecked && r.Reason != "" {
			fmt.Fprintf(&b, "      %-26s why not: %s\n", "", r.Reason)
		}
		fmt.Fprintf(&b, "      %-26s from: %s\n", "", r.Incident)
	}
	if !Failed(results) {
		b.WriteString("\nNo check found a known-bad condition. " +
			"A `?` above is an absence of evidence, not a pass.\n")
	}
	return b.String()
}

// marker is the leading glyph for a verdict, chosen so a not-checked line
// cannot be mistaken at a glance for an ok one.
func marker(s Status) string {
	switch s {
	case StatusOK:
		return "ok"
	case StatusFailed:
		return "FAIL"
	default:
		return "?"
	}
}
