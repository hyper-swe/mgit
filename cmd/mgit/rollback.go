package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"

	"github.com/spf13/cobra"

	"github.com/hyper-swe/mgit/internal/model"
	"github.com/hyper-swe/mgit/internal/service"
)

// rollbackCmd implements mgit rollback. Refs: FR-8.10, MGIT-4.2.1
func rollbackCmd() *cobra.Command {
	var taskID, reason, commitHash, toCommit string
	var dryRun, formatJSON bool

	cmd := &cobra.Command{
		Use:   "rollback [commit-hash]",
		Short: "Rollback task commits (creates revert commit)",
		Args:  cobra.MaximumNArgs(1),
		RunE: func(_ *cobra.Command, args []string) error {
			// --to-commit is an alias for --commit.
			if toCommit != "" && commitHash == "" {
				commitHash = toCommit
			}
			// A positional commit hash is equivalent to --commit, mirroring
			// `mgit show <hash>`; the positional wins if both are supplied.
			commitHash = firstNonEmpty(argAt(args, 0), commitHash)

			// Open the App ONCE and reuse it for both resolving the task from a
			// commit hash and performing the rollback. Opening twice would make the
			// second open contend with the first's still-held process file lock
			// (held until the deferred Close), stalling for the lock timeout. MGIT-25
			app, err := openAppFromCwd()
			if err != nil {
				return err
			}
			defer app.Close()

			ctx := context.Background()

			// --commit / positional hash: resolve the task ID from that commit.
			if commitHash != "" && taskID == "" {
				c, err := app.Commit.GetCommit(ctx, commitHash)
				if err != nil {
					return fmt.Errorf("rollback --commit: resolve commit: %w", err)
				}
				taskID = c.TaskID.String()
			}

			if taskID == "" {
				return fmt.Errorf("a commit hash (positional or --commit) or --task-id is required")
			}

			// In a linked worktree the revert lands on the bound branch, so a
			// rollback there is constrained to the bound task — rolling back a
			// different task would mis-attribute the revert. Refs: MGIT-24
			if app.BoundTask != "" && taskID != app.BoundTask {
				return fmt.Errorf("%w: worktree is bound to task %s, not %s",
					model.ErrTaskMismatch, app.BoundTask, taskID)
			}

			revert, err := app.Rollback.RollbackTask(ctx, service.RollbackRequest{
				TaskID: taskID,
				Reason: reason,
				DryRun: dryRun,
			})
			if err != nil {
				return fmt.Errorf("rollback: %w", err)
			}

			if formatJSON {
				return json.NewEncoder(os.Stdout).Encode(revert)
			}

			if dryRun {
				_, _ = fmt.Fprintf(os.Stdout, "[dry-run] Would create revert: %s\n", revert.Message)
			} else {
				_, _ = fmt.Fprintf(os.Stdout, "[%s] %s\n", revert.ShortID(), revert.Message)
			}
			return nil
		},
	}

	bindTaskIDFlag(cmd, &taskID, "Task to rollback (required unless --commit is set)")
	cmd.Flags().StringVar(&reason, "reason", "", "Reason for rollback")
	cmd.Flags().StringVar(&commitHash, "commit", "", "Rollback by specific commit hash (resolves task ID automatically)")
	cmd.Flags().StringVar(&toCommit, "to-commit", "", "Alias for --commit")
	cmd.Flags().BoolVar(&dryRun, "dry-run", false, "Preview without making changes")
	cmd.Flags().BoolVar(&formatJSON, "json", false, "Output as JSON")
	return cmd
}
