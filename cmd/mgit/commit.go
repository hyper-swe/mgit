package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"

	"github.com/spf13/cobra"

	"github.com/hyper-swe/mgit/internal/model"
	"github.com/hyper-swe/mgit/internal/service"
)

// commitCmd implements mgit commit. Refs: FR-8.3, MGIT-4.1.2, MGIT-77
func commitCmd() *cobra.Command {
	var taskID, message, agentID, sessionID string
	var formatJSON, allowEmpty, dryRun, stageAll bool

	cmd := &cobra.Command{
		Use:   "commit",
		Short: "Create a task-tagged micro-commit",
		Long: "Create a task-tagged micro-commit from the staged changes.\n\n" +
			"Only STAGED changes are recorded. Stage first with `mgit add <path>` " +
			"(or `mgit add -A`), or pass `-a` to stage every change — including new " +
			"files — and commit in one step. A commit that would record nothing is " +
			"refused; pass --allow-empty to create one deliberately.",
		RunE: func(cmd *cobra.Command, _ []string) error {
			// Past flag parsing, a failure is a runtime condition, not misuse:
			// dumping the flag table after it buries the remedy the caller needs.
			// Refs: MGIT-77
			cmd.SilenceUsage = true

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
				_, _ = fmt.Fprintf(os.Stdout,
					"[dry-run] Would commit: task=%s agent=%s message=%q all=%v allow-empty=%v\n",
					taskID, agentID, message, stageAll, allowEmpty)
				return nil
			}

			// -a stages every change (including new files) before committing, so
			// the agent loop is one command per step rather than two. Refs: MGIT-77
			c, err := app.Commit.CreateCommit(ctx, service.CreateCommitRequest{
				TaskID:     taskID,
				AgentID:    agentID,
				SessionID:  sessionID,
				Message:    message,
				StageAll:   stageAll,
				AllowEmpty: allowEmpty,
			})
			if err != nil {
				return commitError(err)
			}

			if formatJSON {
				return json.NewEncoder(os.Stdout).Encode(c)
			}
			_, _ = fmt.Fprintf(os.Stdout, "[%s] %s\n", c.ShortID(), c.Message)
			return nil
		},
	}

	bindTaskIDFlag(cmd, &taskID, "Task ID (required)")
	cmd.Flags().StringVar(&message, "message", "", "Commit message (auto-generated if empty)")
	cmd.Flags().StringVarP(&message, "m", "m", "", "Commit message (shorthand)")
	cmd.Flags().StringVar(&agentID, "agent-id", "cli", "Agent ID")
	cmd.Flags().StringVar(&sessionID, "session-id", "", "Session ID")
	cmd.Flags().BoolVar(&formatJSON, "json", false, "Output as JSON")
	cmd.Flags().BoolVar(&allowEmpty, "allow-empty", false, "Allow commit with no staged changes")
	// No backticks in this usage string: pflag's UnquoteUsage would read the
	// backticked span as the flag's value placeholder and print it as a type.
	cmd.Flags().BoolVarP(&stageAll, "all", "a", false,
		"Stage every change (including new files, same as mgit add -A) then commit")
	cmd.Flags().BoolVar(&dryRun, "dry-run", false, "Validate inputs without creating a commit")
	return cmd
}

// commitError turns a service error into the message a human or agent acts on.
// The service returns the neutral sentinel model.ErrNothingToCommit; naming the
// CLI remedy belongs here, at the surface whose commands the remedy names.
// Refs: FR-8.3, MGIT-77
func commitError(err error) error {
	if errors.Is(err, model.ErrNothingToCommit) {
		return fmt.Errorf("%w — mgit records only STAGED changes.\n"+
			"  Stage them:      mgit add <path>   (or `mgit add -A` for everything)\n"+
			"  Or in one step:  mgit commit -a -m \"<what changed>\"\n"+
			"  Deliberately empty: mgit commit --allow-empty -m \"<why>\"", err)
	}
	return fmt.Errorf("commit: %w", err)
}
