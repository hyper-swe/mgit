package main

import (
	"encoding/json"
	"fmt"
	"io"

	"github.com/spf13/cobra"

	"github.com/hyper-swe/mgit/internal/model"
)

// sandboxSyncCmd re-stages a task's host worktree into its RUNNING sandbox.
//
// The guest's worktree is a staged copy, not a live view (ADR-005, ADR-011),
// and an exec already re-stages automatically. This verb adds the two things
// an exec cannot give you: re-staging WITHOUT running anything in the guest,
// and — with --dry-run — the conflict report, which until now was obtainable
// only by attempting work and being refused.
//
// It adds no mechanism: the daemon routes it through the same host-side
// staging a launch and the pre-exec sync use, so it can never deliver
// something either of those would have refused (SEC-03).
// Refs: MGIT-76, MGIT-71, FR-17.40, ADR-011
func sandboxSyncCmd(connect connectFunc) *cobra.Command {
	var task string
	var force, dryRun, asJSON bool
	cmd := &cobra.Command{
		Use:   "sync --task <id>",
		Short: "Re-stage the host worktree into a task's running sandbox (--dry-run classifies only)",
		Long: "Propagate host worktree changes into a task's running sandbox.\n\n" +
			"An unchanged worktree is a genuine no-op. Paths the guest changed since they\n" +
			"were delivered are a CONFLICT: the sync is refused entirely and every\n" +
			"conflicting path is named. --force overwrites them and reports each one\n" +
			"destroyed. --dry-run reports the same classification without touching the\n" +
			"guest.\n\n" +
			"Not every backend can do this: a sandbox whose worktree was delivered as a\n" +
			"launch-time image must be re-launched to pick up host changes, and says so\n" +
			"rather than reporting a sync that did not happen.",
		Args:          cobra.NoArgs,
		SilenceErrors: true,
		SilenceUsage:  true,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if task == "" {
				return printErr(cmd.ErrOrStderr(), fmt.Errorf("--task-id is required"))
			}
			cl, err := connect(cmd.Context())
			if err != nil {
				return printErr(cmd.ErrOrStderr(), err)
			}
			report, syncErr := cl.SyncWorktree(cmd.Context(), task,
				model.WorktreeSyncOptions{Force: force, DryRun: dryRun})
			return renderSync(cmd, task, report, syncErr, asJSON)
		},
	}
	bindTaskIDFlag(cmd, &task, "task ID whose sandbox receives the host worktree (required)")
	cmd.Flags().BoolVar(&force, "force", false,
		"overwrite paths the guest changed since delivery (each destroyed path is reported)")
	cmd.Flags().BoolVar(&dryRun, "dry-run", false,
		"report what a sync would do — including every conflict — without touching the guest")
	cmd.Flags().BoolVar(&asJSON, "json", false, "output as JSON")
	return cmd
}

// renderSync writes the outcome and returns the command's error.
//
// A refusal prints its classification and STILL returns the error: the paths
// have to be visible, and the exit status has to stay non-zero so a loop that
// chains sync into exec cannot walk past a refused propagation.
func renderSync(cmd *cobra.Command, task string, report *model.WorktreeSyncReport, syncErr error, asJSON bool) error {
	if asJSON {
		if report != nil {
			_ = json.NewEncoder(cmd.OutOrStdout()).Encode(report)
		}
		if syncErr != nil {
			return printErr(cmd.ErrOrStderr(), syncErr)
		}
		return nil
	}
	if syncErr != nil {
		writeSyncConflicts(cmd.ErrOrStderr(), report)
		return printErr(cmd.ErrOrStderr(), syncErr)
	}
	writeSyncReport(cmd.OutOrStdout(), task, report)
	return nil
}

// writeSyncReport renders a successful sync, or a dry run's projection.
func writeSyncReport(w io.Writer, task string, report *model.WorktreeSyncReport) {
	if report == nil {
		report = &model.WorktreeSyncReport{}
	}
	switch {
	case report.Skipped && report.Detail != "":
		_, _ = fmt.Fprintf(w, "Nothing to sync for task %s: %s\n", task, report.Detail)
		return
	case report.Skipped:
		_, _ = fmt.Fprintf(w,
			"Sandbox for task %s is already up to date (host worktree unchanged since delivery)\n", task)
		return
	case report.DryRun:
		writeSyncDryRun(w, task, report)
	default:
		_, _ = fmt.Fprintf(w, "Synced host worktree into task %s's sandbox: %s\n", task, syncCounts(report))
	}
	writeSyncPaths(w, report)
	writeSyncTruncation(w, report)
}

// writeSyncDryRun renders the projection headline, which says plainly whether
// a real sync would be refused — the answer the query exists to give.
func writeSyncDryRun(w io.Writer, task string, report *model.WorktreeSyncReport) {
	if report.Refused {
		_, _ = fmt.Fprintf(w, "Dry run for task %s — a sync would be REFUSED: %d conflicting path(s)\n",
			task, len(report.Conflicts))
		return
	}
	_, _ = fmt.Fprintf(w, "Dry run for task %s — a sync would apply: %s\n", task, syncCounts(report))
}

// syncCounts summarizes a report's classes in one line.
func syncCounts(report *model.WorktreeSyncReport) string {
	// The TOTALS, not the list lengths: on a large tree the lists are bounded
	// and would under-report. Refs: MGIT-160
	updated := reportTotal(report.UpdatedTotal, len(report.Updated))
	deleted := reportTotal(report.DeletedTotal, len(report.Deleted))
	overridden := reportTotal(report.OverriddenTotal, len(report.Overridden))
	out := fmt.Sprintf("%d updated, %d deleted", updated, deleted)
	if overridden > 0 {
		out += fmt.Sprintf(", %d overwritten (un-landed guest changes destroyed)", overridden)
	}
	return out
}

// reportTotal takes whichever of the declared total and the list length is
// larger.
//
// The totals are new (MGIT-160), so a report from an OLDER daemon carries
// none, and reading them blindly would print "0 updated" beside a list of
// updates — a silently wrong count, which is the exact class of failure this
// work exists to remove, reintroduced by its own fix. Taking the larger is
// safe in both directions: it can never under-report a bounded list, and it
// can never invent a total a peer did not send. Refs: MGIT-160
func reportTotal(declared, listed int) int {
	if declared > listed {
		return declared
	}
	return listed
}

// writeSyncTruncation states that the listing below is partial, when it is.
// Silence here would let a shortened list read as the whole story — the one
// outcome worse than the crash this replaced, because a wrong answer is
// believed and a crash is not. Refs: MGIT-160
func writeSyncTruncation(w io.Writer, report *model.WorktreeSyncReport) {
	if !report.Truncated {
		return
	}
	_, _ = fmt.Fprintf(w, "  ... listing truncated to %d paths per class; the counts above are complete.\n",
		model.SyncReportPathLimit)
}

// writeSyncPaths lists every path the report names, so "2 updated" is never
// the whole story a reviewer gets.
//
// A path that --force overwrote is labeled "overwritten" and carries the
// guest change it destroyed, rather than being listed twice — once as an
// update and again as an unresolved conflict.
func writeSyncPaths(w io.Writer, report *model.WorktreeSyncReport) {
	reasons := overriddenReasons(report)
	for _, p := range report.Updated {
		if reason, wasForced := reasons[p]; wasForced {
			_, _ = fmt.Fprintf(w, "  %-11s %s (%s)\n", "overwritten", p, reason)
			continue
		}
		_, _ = fmt.Fprintf(w, "  %-11s %s\n", "update", p)
	}
	for _, p := range report.Deleted {
		_, _ = fmt.Fprintf(w, "  %-11s %s\n", "delete", p)
	}
	writeSyncConflicts(w, report)
}

// overriddenReasons maps each --force-overwritten path to the guest change it
// destroyed.
func overriddenReasons(report *model.WorktreeSyncReport) map[string]string {
	out := make(map[string]string, len(report.Overridden))
	for _, p := range report.Overridden {
		out[p] = "un-landed guest change destroyed"
	}
	for _, c := range report.Conflicts {
		if _, ok := out[c.Path]; ok {
			out[c.Path] = c.Reason
		}
	}
	return out
}

// writeSyncConflicts names every path that BLOCKS a sync, and its reason. A
// refusal that says only "conflict" gives a caller nothing to act on, so the
// remedy is spelled out too.
//
// Paths --force already overwrote are excluded: they no longer block anything,
// and offering "--force" as the remedy for a sync that just forced its way
// through would be advice to repeat what already happened.
// Refs: MGIT-71, MGIT-76
func writeSyncConflicts(w io.Writer, report *model.WorktreeSyncReport) {
	if report == nil {
		return
	}
	overridden := overriddenReasons(report)
	blocking := 0
	for _, c := range report.Conflicts {
		if _, wasForced := overridden[c.Path]; wasForced {
			continue
		}
		blocking++
		_, _ = fmt.Fprintf(w, "  %-11s %s (%s)\n", "conflict", c.Path, c.Reason)
	}
	if blocking == 0 {
		return
	}
	_, _ = fmt.Fprintln(w,
		"  land the guest's work, or re-run with --force to overwrite it (every overwritten path is reported)")
}
