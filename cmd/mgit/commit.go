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

// commitCmd implements mgit commit. Refs: FR-8.3, MGIT-4.1.2
func commitCmd() *cobra.Command {
	var taskID, message, agentID, sessionID string
	var formatJSON, allowEmpty, dryRun bool

	cmd := &cobra.Command{
		Use:   "commit",
		Short: "Create a task-tagged micro-commit",
		RunE: func(_ *cobra.Command, _ []string) error {
			app, err := openAppFromCwd()
			if err != nil {
				return err
			}
			defer app.Close()

			// Inside a linked worktree, commits auto-inherit the bound task ID
			// (CLAUDE.md); an explicit --task-id that contradicts the binding is
			// rejected. Refs: FR-16, MGIT-24
			if app.BoundTask != "" {
				switch taskID {
				case "":
					taskID = app.BoundTask
				case app.BoundTask:
				default:
					return fmt.Errorf("%w: worktree is bound to task %s, not %s",
						model.ErrTaskMismatch, app.BoundTask, taskID)
				}
			}
			if taskID == "" {
				return fmt.Errorf("--task-id is required")
			}

			ctx := context.Background()

			// --dry-run: validate inputs but do not create the commit.
			if dryRun {
				_, _ = fmt.Fprintf(os.Stdout, "[dry-run] Would commit: task=%s agent=%s message=%q allow-empty=%v\n",
					taskID, agentID, message, allowEmpty)
				return nil
			}

			c, err := app.Commit.CreateCommit(ctx, service.CreateCommitRequest{
				TaskID:    taskID,
				AgentID:   agentID,
				SessionID: sessionID,
				Message:   message,
			})
			if err != nil {
				return fmt.Errorf("commit: %w", err)
			}

			if formatJSON {
				return json.NewEncoder(os.Stdout).Encode(c)
			}
			_, _ = fmt.Fprintf(os.Stdout, "[%s] %s\n", c.ShortID(), c.Message)
			return nil
		},
	}

	cmd.Flags().StringVar(&taskID, "task-id", "", "Task ID (required)")
	cmd.Flags().StringVar(&message, "message", "", "Commit message (auto-generated if empty)")
	cmd.Flags().StringVarP(&message, "m", "m", "", "Commit message (shorthand)")
	cmd.Flags().StringVar(&agentID, "agent-id", "cli", "Agent ID")
	cmd.Flags().StringVar(&sessionID, "session-id", "", "Session ID")
	cmd.Flags().BoolVar(&formatJSON, "json", false, "Output as JSON")
	cmd.Flags().BoolVar(&allowEmpty, "allow-empty", false, "Allow commit with no staged changes")
	cmd.Flags().BoolVar(&dryRun, "dry-run", false, "Validate inputs without creating a commit")
	return cmd
}
