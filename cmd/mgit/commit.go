package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/hyper-swe/mgit/internal/model"
	"github.com/hyper-swe/mgit/internal/service"
	gitstore "github.com/hyper-swe/mgit/internal/store/git"
)

// commitCmd implements mgit commit. Refs: FR-8.3, MGIT-4.1.2, MGIT-77, MGIT-105
func commitCmd() *cobra.Command {
	var taskID, message, messageFile, agentID, sessionID string
	var paragraphs []string
	var formatJSON, allowEmpty, dryRun, stageAll, allowLarge bool

	cmd := &cobra.Command{
		Use:   "commit",
		Args:  refusePaths("commit", commitPathRemedy),
		Short: "Create a task-tagged micro-commit",
		Long: "Create a task-tagged micro-commit from the staged changes.\n\n" +
			"Only STAGED changes are recorded. Stage first with `mgit add <path>` " +
			"(or `mgit add -A`), or pass `-a` to stage every change — including new " +
			"files — and commit in one step. A commit that would record nothing is " +
			"refused; pass --allow-empty to create one deliberately.\n\n" +
			"The message comes from -m/--message inline, or from --file/-F, which " +
			"reads it verbatim from a file (or from stdin when the path is -) with " +
			"no shell involved. Repeat -m to join messages as separate paragraphs. " +
			"The two sources are mutually exclusive.",
		RunE: func(cmd *cobra.Command, _ []string) error {
			// Past flag parsing, a failure is a runtime condition, not misuse:
			// dumping the flag table after it buries the remedy the caller needs.
			// Refs: MGIT-77
			cmd.SilenceUsage = true

			// Resolve the message BEFORE anything is opened, staged or written:
			// an unreadable message file must commit nothing. Refs: MGIT-105
			resolved, err := resolveMessage(cmd, "commit", strings.Join(paragraphs, "\n\n"), messageFile)
			if err != nil {
				return err
			}
			message = resolved

			app, err := openAppFromCwd()
			if err != nil {
				return err
			}
			defer app.Close()

			taskID, err = resolveCommitTaskID(app.BoundTask, taskID)
			if err != nil {
				return err
			}

			ctx := context.Background()

			agentID, err = resolveCommitAgentID(ctx, app, cmd.Flags().Changed("agent-id"), agentID)
			if err != nil {
				return err
			}

			// --dry-run: validate inputs but do not create the commit.
			if dryRun {
				printCommitDryRun(taskID, agentID, message, stageAll, allowEmpty)
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
				AllowLarge: allowLarge,
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
	cmd.Flags().StringArrayVarP(&paragraphs, "message", "m", nil,
		"Commit message (repeat for separate paragraphs; auto-generated if empty)")
	bindMessageFileFlag(cmd, &messageFile, "commit")
	cmd.Flags().StringVar(&agentID, "agent-id", "cli", "Agent ID")
	cmd.Flags().StringVar(&sessionID, "session-id", "", "Session ID")
	cmd.Flags().BoolVar(&formatJSON, "json", false, "Output as JSON")
	cmd.Flags().BoolVar(&allowEmpty, "allow-empty", false, "Allow commit with no staged changes")
	// No backticks in this usage string: pflag's UnquoteUsage would read the
	// backticked span as the flag's value placeholder and print it as a type.
	cmd.Flags().BoolVarP(&stageAll, "all", "a", false,
		"Stage every change (including new files, same as mgit add -A) then commit")
	cmd.Flags().BoolVar(&dryRun, "dry-run", false, "Validate inputs without creating a commit")
	// The escape hatch for the -a size tripwire (MGIT-131). Named in the
	// refusal itself, so a caller who hits the guard is told how to proceed.
	cmd.Flags().BoolVar(&allowLarge, "allow-large", false,
		"With -a, stage files over the limits.max_staged_file_mb size limit")
	return cmd
}

// resolveCommitAgentID picks the agent a commit is recorded under. An explicit
// --agent-id wins. Otherwise a commit from inside a task-bound worktree takes the
// agent that worktree is bound to, the way it takes the worktree's task; with no
// worktree, or a worktree made without an agent, nothing better is known and the
// flag's default ("cli") stands. A failure to read the worktree is returned, not
// swallowed into the default: attributing a commit to the wrong agent silently is
// the thing this exists to stop. Refs: MGIT-117, MGIT-116
func resolveCommitAgentID(ctx context.Context, app *App, explicit bool, flagValue string) (string, error) {
	if explicit || app.BoundTask == "" {
		return flagValue, nil
	}
	wt, err := service.NewWorktreeService(app.Index, app.Branch, gitstore.NewWorktreeStore(app.Repo), func() time.Time { return time.Now().UTC() }).ByTask(ctx, app.BoundTask)
	switch {
	case err == nil && wt.AgentID != "":
		return wt.AgentID, nil
	case err == nil, errors.Is(err, model.ErrWorktreeNotFound):
		return flagValue, nil
	default:
		return "", fmt.Errorf("resolve the worktree's agent: %w", err)
	}
}

// printCommitDryRun reports what --dry-run would have recorded, including the
// resolved message — so a message read from a file can be inspected before it
// is committed. Refs: FR-8.3, MGIT-105
func printCommitDryRun(taskID, agentID, message string, stageAll, allowEmpty bool) {
	_, _ = fmt.Fprintf(os.Stdout,
		"[dry-run] Would commit: task=%s agent=%s message=%q all=%v allow-empty=%v\n",
		taskID, agentID, message, stageAll, allowEmpty)
}

// resolveCommitTaskID returns the task ID a commit is attributed to. Inside a
// linked worktree, commits auto-inherit the bound task ID (CLAUDE.md); an
// explicit --task-id that contradicts the binding is rejected rather than
// silently re-attributed. Outside one, --task-id is required.
// Refs: FR-16, MGIT-24
func resolveCommitTaskID(boundTask, taskID string) (string, error) {
	if boundTask != "" {
		switch taskID {
		case "", boundTask:
			return boundTask, nil
		default:
			return "", fmt.Errorf("%w: worktree is bound to task %s, not %s",
				model.ErrTaskMismatch, boundTask, taskID)
		}
	}
	if taskID == "" {
		return "", errors.New("--task-id is required")
	}
	return taskID, nil
}

// commitError turns a service error into the message a human or agent acts on.
// The service returns the neutral sentinel model.ErrNothingToCommit; naming the
// CLI remedy belongs here, at the surface whose commands the remedy names.
// Refs: FR-8.3, MGIT-77
func commitError(err error) error {
	// The size refusal (MGIT-131) is already a complete, self-contained
	// message: file, size, limit, and both overrides. A "commit:" prefix adds
	// nothing and separates the reader from the remedy.
	if errors.Is(err, model.ErrFileTooLarge) {
		return err
	}
	if errors.Is(err, model.ErrNothingToCommit) {
		return fmt.Errorf("%w — mgit records only STAGED changes.\n"+
			"  Stage them:      mgit add <path>   (or `mgit add -A` for everything)\n"+
			"  Or in one step:  mgit commit -a -m \"<what changed>\"\n"+
			"  Deliberately empty: mgit commit --allow-empty -m \"<why>\"", err)
	}
	return fmt.Errorf("commit: %w", err)
}
