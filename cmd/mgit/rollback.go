package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"

	"github.com/spf13/cobra"

	"github.com/hyper-swe/mgit-dev/internal/service"
)

// rollbackCmd implements mgit rollback. Refs: FR-8.10, MGIT-4.2.1
func rollbackCmd() *cobra.Command {
	var taskID, reason, commitHash, toCommit string
	var dryRun, formatJSON bool

	cmd := &cobra.Command{
		Use:   "rollback",
		Short: "Rollback task commits (creates revert commit)",
		RunE: func(_ *cobra.Command, _ []string) error {
			// --to-commit is an alias for --commit.
			if toCommit != "" && commitHash == "" {
				commitHash = toCommit
			}

			// --commit: resolve task ID from a specific commit hash.
			if commitHash != "" && taskID == "" {
				app, err := openAppFromCwd()
				if err != nil {
					return err
				}
				defer app.Close()

				ctx := context.Background()
				c, err := app.Commit.GetCommit(ctx, commitHash)
				if err != nil {
					return fmt.Errorf("rollback --commit: resolve commit: %w", err)
				}
				taskID = c.TaskID.String()
			}

			if taskID == "" {
				return fmt.Errorf("--task-id or --commit is required")
			}

			app, err := openAppFromCwd()
			if err != nil {
				return err
			}
			defer app.Close()

			ctx := context.Background()
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

	cmd.Flags().StringVar(&taskID, "task-id", "", "Task to rollback (required unless --commit is set)")
	cmd.Flags().StringVar(&reason, "reason", "", "Reason for rollback")
	cmd.Flags().StringVar(&commitHash, "commit", "", "Rollback by specific commit hash (resolves task ID automatically)")
	cmd.Flags().StringVar(&toCommit, "to-commit", "", "Alias for --commit")
	cmd.Flags().BoolVar(&dryRun, "dry-run", false, "Preview without making changes")
	cmd.Flags().BoolVar(&formatJSON, "json", false, "Output as JSON")
	return cmd
}
