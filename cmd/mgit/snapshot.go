package main

import (
	"encoding/json"
	"fmt"
	"time"

	"github.com/spf13/cobra"

	"github.com/hyper-swe/mgit/internal/model"
	gitstore "github.com/hyper-swe/mgit/internal/store/git"
)

// snapshotCmd exposes the PASSIVE worktree snapshots the daemon records.
//
// It is a separate verb from `log` on purpose, and that separation is the
// feature rather than a filing decision. R-H234 splits what the SYSTEM KNOWS
// (the worktree held these bytes at this time) from what the AGENT CLAIMS
// (this was a coherent step), because a reviewer who cannot tell them apart is
// reading a record that looks more authoritative than it is. Rendering a
// snapshot next to authored commits would be, in MGIT-110's words, "a worse
// lie than no autosave". Refs: MGIT-110, R-H234
func snapshotCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "snapshot",
		Short: "Passive worktree snapshots the system recorded (not authored commits)",
		Long: "Snapshots are captured by mgit itself when a task's worktree stops changing. " +
			"They require nothing of the agent, they are never squashed or landed, and they " +
			"never appear in `mgit log` — they are evidence of when work changed, kept " +
			"separate from the agent's account of what it meant to do.",
	}
	cmd.AddCommand(snapshotListCmd(), snapshotRestoreCmd())
	return cmd
}

// snapshotListCmd lists a task's snapshots, newest first. Refs: MGIT-110
func snapshotListCmd() *cobra.Command {
	var taskID string
	var asJSON bool
	cmd := &cobra.Command{
		Use:   "list",
		Short: "List the passive snapshots recorded for a task",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			app, err := openAppFromCwd()
			if err != nil {
				return err
			}
			defer app.Close()
			task, err := resolveSnapshotTask(app, taskID)
			if err != nil {
				return err
			}
			snaps, err := gitstore.NewSnapshotStore(app.Repo).List(cmd.Context(), task)
			if err != nil {
				return err
			}
			return renderSnapshots(cmd, task, snaps, asJSON)
		},
	}
	bindTaskIDFlag(cmd, &taskID, "task whose snapshots to list (defaults to this worktree's bound task)")
	cmd.Flags().BoolVar(&asJSON, "json", false, "emit JSON")
	return cmd
}

// renderSnapshots prints the snapshot list, or states plainly that there are
// none. "No snapshots" is reported as an ABSENCE OF EVIDENCE, never as
// "nothing changed": the daemon may not have been running, and a reader who
// takes silence for a clean bill has been misled. Refs: MGIT-110
func renderSnapshots(cmd *cobra.Command, taskID string, snaps []model.Snapshot, asJSON bool) error {
	out := cmd.OutOrStdout()
	if asJSON {
		return json.NewEncoder(out).Encode(map[string]any{"task_id": taskID, "snapshots": snaps})
	}
	if len(snaps) == 0 {
		_, _ = fmt.Fprintf(out, "No passive snapshots recorded for %s.\n", taskID)
		_, _ = fmt.Fprintf(out, "That means none were TAKEN — not that nothing changed. "+
			"Snapshots are recorded by the sandbox daemon, so a task run without `--sandbox`, "+
			"or one whose daemon was not running, has none.\n")
		return nil
	}
	_, _ = fmt.Fprintf(out, "Passive snapshots for %s (system-recorded; not authored commits):\n", taskID)
	for _, s := range snaps {
		_, _ = fmt.Fprintf(out, "  %s  %s  %d files  (%s)\n",
			s.ID, s.CapturedAt.Format(time.RFC3339), s.FileCount, s.Trigger)
	}
	_, _ = fmt.Fprintf(out, "\nRecover one into a NEW directory:\n  mgit snapshot restore <id> --to <dir>\n")
	return nil
}

// snapshotRestoreCmd materializes a snapshot into a new directory. Refs: MGIT-110
func snapshotRestoreCmd() *cobra.Command {
	var dest string
	cmd := &cobra.Command{
		Use:   "restore <snapshot-id>",
		Short: "Materialize a snapshot into a new directory",
		Long: "Writes the snapshot's files into --to, which must be empty or not exist. " +
			"It deliberately never restores over a live worktree: the work a snapshot " +
			"exists to protect is usually still on disk there, and overwriting it would " +
			"destroy exactly what was being recovered. Compare the two copies yourself.",
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if dest == "" {
				return fmt.Errorf("--to <dir> is required: a snapshot is restored into a NEW directory, " +
					"never over the worktree it came from")
			}
			app, err := openAppFromCwd()
			if err != nil {
				return err
			}
			defer app.Close()
			files, err := gitstore.NewSnapshotStore(app.Repo).Materialize(cmd.Context(), args[0], dest)
			if err != nil {
				return err
			}
			_, _ = fmt.Fprintf(cmd.OutOrStdout(),
				"Restored snapshot %s: %d files into %s\n", args[0], files, dest)
			return nil
		},
	}
	cmd.Flags().StringVar(&dest, "to", "", "destination directory (must be empty or not exist)")
	return cmd
}

// resolveSnapshotTask falls back to the worktree's bound task, so an agent
// standing in its own worktree needs no flag. Refs: MGIT-110, FR-16
func resolveSnapshotTask(app *App, taskID string) (string, error) {
	if taskID != "" {
		return taskID, nil
	}
	if app.BoundTask != "" {
		return app.BoundTask, nil
	}
	return "", fmt.Errorf("--task-id is required (this directory is not bound to a task)")
}
