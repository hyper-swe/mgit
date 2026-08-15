package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"

	"github.com/hyper-swe/mgit/internal/model"
	"github.com/hyper-swe/mgit/internal/store/index"
)

// taskLogJSON is the --json shape of `mgit log --task-id`.
//
// The commit records travel under "commits" and the evidence label under
// "cadence". The label lives HERE, on the command that lists a task's trail,
// because that listing is the thing that misleads: a reviewer reading seven
// commits believes they are seeing seven coherent steps (MGIT-110). It is
// deliberately not bolted onto `mgit verify`, which answers a different
// question — whether the record is INTACT, not whether it is what it appears
// to be. Blurring the two would weaken both.
// Refs: MGIT-110, R-H234, FR-4
type taskLogJSON struct {
	TaskID  string                 `json:"task_id"`
	Commits []index.CommitRecord   `json:"commits"`
	Cadence *model.CadenceEvidence `json:"cadence"`
}

// cadenceTokenDoc documents the stable tokens on `mgit log --help`, where an
// integrator about to script the command reads them — the placement MGIT-109
// settled on for the policy failure codes. Refs: MGIT-110, MGIT-109, R-H234
const cadenceTokenDoc = "\n\nCOMMIT-CADENCE EVIDENCE (stable contract). With --task-id, the trail carries a\n" +
	"mechanical label saying whether it reads as process history or as a trail\n" +
	"packaged at hand-off. Match on the TOKEN — in `cadence.verdict` under --json, and\n" +
	"at the start of the human footer — never on the summary prose, which will change.\n\n" +
	"  PACKAGED_POST_HOC      the commits sit inside a small slice at the end of the\n" +
	"                         run. Read the trail as one step recorded in N parts.\n" +
	"  SPREAD_ACROSS_RUN      the commits are distributed through the run. Consistent\n" +
	"                         with process history; not proof of it.\n" +
	"  SINGLE_CHECKPOINT      one commit closed the run and nothing came before it: a\n" +
	"                         result, with no steps recorded on the way to it.\n" +
	"  NO_COMMITS             nothing was recorded for this task at all.\n" +
	"  INSUFFICIENT_EVIDENCE  the RUN could not be measured, so no verdict is honest.\n" +
	"                         Always carries a `reason`, and NEVER means \"fine\":\n\n" +
	"      NO_DENOMINATOR            no worktree registered, so nothing says when the\n" +
	"                                run began (`mgit work` registers one).\n" +
	"      COMMITS_PRECEDE_WORKTREE  commits older than the worktree holding them.\n" +
	"      RUN_TOO_LONG              the trail spans more than one plausible run.\n" +
	"      RUN_TOO_SHORT             too short a run to read anything into its shape.\n\n" +
	"Every refusal is a failure of the DENOMINATOR; the commit COUNT is never one of\n" +
	"them. Zero commits and one commit are complete observations with their own\n" +
	"verdicts, and they are the common case: across the six runs this was measured on,\n" +
	"half produced one commit or none, and only one produced a manufactured trail.\n\n" +
	"The label is EVIDENCE, not a score. Nothing gates on it, and nothing should: an\n" +
	"agent committing to satisfy a checker manufactures the very trail it exposes.\n" +
	"Squash and merge commits are excluded from the trail — they restate existing work\n" +
	"at hand-off, so counting them drags the window to the end of the run."

// runTaskLog prints one task's commit trail together with its commit-cadence
// evidence label. Refs: FR-4, FR-8.4, MGIT-110
func runTaskLog(ctx context.Context, app *App, taskID string, asJSON bool) error {
	records, err := app.Commit.GetTaskCommits(ctx, taskID)
	if err != nil {
		return fmt.Errorf("log: %w", err)
	}
	cadence, err := app.Commit.TaskCadence(ctx, taskID)
	if err != nil {
		return fmt.Errorf("log: %w", err)
	}

	if asJSON {
		if records == nil {
			records = []index.CommitRecord{}
		}
		return json.NewEncoder(os.Stdout).Encode(taskLogJSON{
			TaskID: taskID, Commits: records, Cadence: cadence,
		})
	}

	for _, r := range records {
		_, _ = fmt.Fprintf(os.Stdout, "%s [%s] pos=%d\n", shortHash(r.CommitHash), r.TaskID, r.Position)
	}
	printCadence(os.Stdout, cadence)
	return nil
}

// printCadence writes the evidence footer. The verdict token leads the line so
// it is readable without --json, and the measurement follows so a reviewer can
// disagree with the arithmetic rather than only with the word.
// Refs: MGIT-110, MGIT-109
func printCadence(w io.Writer, ev *model.CadenceEvidence) {
	if ev == nil {
		return
	}
	_, _ = fmt.Fprintf(w, "\ncadence: %s", ev.Verdict)
	if ev.Reason != "" {
		_, _ = fmt.Fprintf(w, " (%s)", ev.Reason)
	}
	_, _ = fmt.Fprintf(w, "\n  %s\n", ev.Summary)
	if m := ev.Measure; m != nil {
		_, _ = fmt.Fprintf(w, "  measured %s to %s, from the worktree created at %s [%s]\n",
			m.FirstCommitAt, m.LastCommitAt, m.RunStartAt, m.Denominator)
	}
}

// shortHash abbreviates a commit hash for display without assuming a length.
func shortHash(hash string) string {
	if len(hash) <= 8 {
		return hash
	}
	return hash[:8]
}
