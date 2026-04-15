package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"

	"github.com/spf13/cobra"

	"github.com/hyper-swe/mgit/internal/model"
	gitstore "github.com/hyper-swe/mgit/internal/store/git"
)

// diffCmd implements `mgit diff`. Refs: FR-8.5, FR-11, MGIT-4.1.4
//
// Usage:
//
//	mgit diff --from HASH --to HASH       # diff between two commits
//	mgit diff --task-id ID                # cumulative diff for a task
//	mgit diff --from HASH                 # diff from HASH to current HEAD
//	mgit diff --stat                       # show statistics only
//	mgit diff --unified N                  # number of context lines (informational)
//	mgit diff --json                       # structured JSON output
func diffCmd() *cobra.Command {
	var fromHash, toHash, taskID string
	var stat, staged bool
	var unified int
	var formatJSON bool

	cmd := &cobra.Command{
		Use:   "diff",
		Short: "Show differences between commits or for a task",
		RunE: func(_ *cobra.Command, _ []string) error {
			app, err := openAppFromCwd()
			if err != nil {
				return err
			}
			defer app.Close()

			ctx := context.Background()
			_ = unified // accepted for compatibility; informational only

			// --staged: show status of staged files instead of commit diff.
			if staged {
				ws := gitstore.NewWorktreeStore(app.Repo)
				files, err := ws.Status(ctx)
				if err != nil {
					return fmt.Errorf("diff --staged: %w", err)
				}
				if formatJSON {
					return json.NewEncoder(os.Stdout).Encode(files)
				}
				for _, f := range files {
					if f.Staging != " " && f.Staging != "?" {
						_, _ = fmt.Fprintf(os.Stdout, "%s %s\n", f.Staging, f.Path)
					}
				}
				return nil
			}

			// --task-id mode: cumulative diff for a task
			if taskID != "" {
				diffs, err := app.Diff.DiffTask(ctx, taskID)
				if err != nil {
					return fmt.Errorf("diff task: %w", err)
				}
				return renderDiff(app, diffs, stat, formatJSON)
			}

			// --from required for commit diff
			if fromHash == "" {
				return fmt.Errorf("--from or --task-id is required")
			}

			// Default --to to current HEAD if not provided
			if toHash == "" {
				head, err := app.Repo.Head()
				if err != nil {
					return fmt.Errorf("resolve HEAD: %w", err)
				}
				toHash = head
			}

			diffs, err := app.Diff.DiffCommits(ctx, fromHash, toHash)
			if err != nil {
				return fmt.Errorf("diff commits: %w", err)
			}
			return renderDiff(app, diffs, stat, formatJSON)
		},
	}

	cmd.Flags().StringVar(&fromHash, "from", "", "From commit hash")
	cmd.Flags().StringVar(&toHash, "to", "", "To commit hash (default: HEAD)")
	cmd.Flags().StringVar(&taskID, "task-id", "", "Cumulative diff for a task")
	cmd.Flags().BoolVar(&stat, "stat", false, "Show statistics only")
	cmd.Flags().IntVar(&unified, "unified", 3, "Number of context lines (informational)")
	cmd.Flags().BoolVar(&formatJSON, "json", false, "Output as JSON")
	cmd.Flags().BoolVar(&staged, "staged", false, "Show status of staged files")
	return cmd
}

// renderDiff formats and writes a slice of FileDiffs to stdout in the
// requested format. JSON takes precedence over --stat.
// Refs: FR-11
func renderDiff(app *App, diffs []model.FileDiff, stat, formatJSON bool) error {
	if formatJSON {
		return json.NewEncoder(os.Stdout).Encode(diffs)
	}
	if stat {
		_, err := fmt.Fprint(os.Stdout, app.Diff.FormatStat(diffs))
		return err
	}
	_, err := fmt.Fprint(os.Stdout, app.Diff.FormatUnified(diffs))
	return err
}
