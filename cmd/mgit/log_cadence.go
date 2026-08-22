package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/hyper-swe/mgit/internal/model"
	gitstore "github.com/hyper-swe/mgit/internal/store/git"
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
	Commits []taskLogCommitJSON    `json:"commits"`
	Cadence *model.CadenceEvidence `json:"cadence"`
}

// taskLogCommitJSON is one commit in the --json trail: every field the index
// records, plus the SUBJECT read from the commit object.
//
// The embedded record keeps the existing shape byte-for-byte, so an
// integration reading `commits[].commit_hash` today is unaffected; `subject`
// is additive. It exists because a machine reader deserves the same facts as a
// human one — a trail whose JSON could only report positions would leave an
// integrator re-deriving messages one `mgit show` at a time, which is the
// friction this view exists to remove. Refs: MGIT-155
type taskLogCommitJSON struct {
	index.CommitRecord
	// Subject is empty when the commit object could not be read. Absent rather
	// than invented: a machine reader can tell "no subject recorded" from "the
	// subject is the empty string" only if we never fabricate one.
	Subject string `json:"subject,omitempty"`
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
		commits := make([]taskLogCommitJSON, 0, len(records))
		for _, r := range records {
			commits = append(commits, taskLogCommitJSON{
				CommitRecord: r, Subject: subjectFor(ctx, app, r.CommitHash),
			})
		}
		return json.NewEncoder(os.Stdout).Encode(taskLogJSON{
			TaskID: taskID, Commits: commits, Cadence: cadence,
		})
	}

	for _, r := range records {
		_, _ = fmt.Fprintln(os.Stdout,
			taskLogLine(shortHash(r.CommitHash), r.TaskID, subjectFor(ctx, app, r.CommitHash)))
	}
	printCadence(os.Stdout, cadence)
	return nil
}

// subjectWidth bounds a subject so one commit stays one line. A trail is read
// by scanning it, and a wrapped 200-character subject destroys that.
const subjectWidth = 72

// taskLogLine renders one line of a task's trail.
//
// It carries the SUBJECT, not the index position. `mgit log --task-id` is the
// reviewer's view of what an agent did, and it printed `pos=0 pos=1 pos=2` —
// which communicates nothing about the work and made the reviewer run
// `mgit show` once per commit, exactly the friction this view exists to
// remove. It also undercut the cadence label printed beneath it, which
// characterizes commits the reader could not identify.
//
// A missing subject is STATED rather than left blank: a blank would read as a
// commit with an empty message, which is a different fact. Refs: MGIT-155, MGIT-110
func taskLogLine(shortHash, taskID, subject string) string {
	if subject == "" {
		subject = "(message unavailable)"
	}
	return fmt.Sprintf("%s [%s] %s", shortHash, taskID, subject)
}

// subjectFor reads a commit's subject from the commit OBJECT — the
// authoritative copy — rather than from a new index column.
//
// No schema change and no second copy of the message: a duplicate could drift
// from the original, and the trail would then describe commits that no longer
// say what it claims. An unreadable object yields "" and is reported as an
// absence by taskLogLine. Refs: MGIT-155
func subjectFor(ctx context.Context, app *App, hash string) string {
	c, err := gitstore.NewCommitStore(app.Repo).GetCommit(ctx, hash)
	if err != nil || c == nil {
		return ""
	}
	return commitSubject(c.Message)
}

// stripTaskMarker removes a leading `[MGIT:<task>] ` marker.
//
// Only that exact shape, and only at the front: a subject that merely mentions
// a bracketed token keeps it, because rewriting a message beyond its known
// prefix would be editing the author's words rather than de-duplicating our
// own. Refs: MGIT-155
func stripTaskMarker(line string) string {
	if !strings.HasPrefix(line, "[MGIT:") {
		return line
	}
	end := strings.Index(line, "] ")
	if end < 0 {
		return line
	}
	return strings.TrimSpace(line[end+2:])
}

// commitSubject is the first non-empty line of a commit message, bounded to
// one line's worth, with the task marker stripped.
//
// mgit embeds the task in the message as `[MGIT:<task>] <subject>`, and the
// log line already carries the task in its own column — so leaving the marker
// in printed it twice on every line, which is noise in exactly the column a
// reviewer is scanning. Caught by reading real output, not by a unit test.
// Refs: MGIT-155
func commitSubject(message string) string {
	for _, line := range strings.Split(message, "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		line = stripTaskMarker(line)
		if len(line) > subjectWidth {
			return line[:subjectWidth-1] + "\u2026"
		}
		return line
	}
	return ""
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
