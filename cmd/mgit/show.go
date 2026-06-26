package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"time"

	"github.com/spf13/cobra"
)

// showCmd implements mgit show. Refs: FR-8.7, MGIT-4.1.6
func showCmd() *cobra.Command {
	var taskID, format string
	var formatJSON, stat bool

	cmd := &cobra.Command{
		Use:   "show [commit-hash]",
		Short: "Show commit details",
		Args:  cobra.MaximumNArgs(1),
		RunE: func(_ *cobra.Command, args []string) error {
			app, err := openAppFromCwd()
			if err != nil {
				return err
			}
			defer app.Close()

			ctx := context.Background()

			// --task-id mode: show task info instead of a single commit.
			if taskID != "" {
				records, err := app.Commit.GetTaskCommits(ctx, taskID)
				if err != nil {
					return fmt.Errorf("show task: %w", err)
				}
				if formatJSON || format == "json" {
					return json.NewEncoder(os.Stdout).Encode(records)
				}
				_, _ = fmt.Fprintf(os.Stdout, "Task %s: %d commits\n", taskID, len(records))
				for _, r := range records {
					_, _ = fmt.Fprintf(os.Stdout, "  %s pos=%d %s\n", r.CommitHash[:8], r.Position, r.CreatedAt)
				}
				return nil
			}

			if len(args) == 0 {
				return fmt.Errorf("commit hash or --task-id is required")
			}

			c, err := app.Commit.GetCommit(ctx, args[0])
			if err != nil {
				return fmt.Errorf("show: %w", err)
			}

			if formatJSON || format == "json" {
				return json.NewEncoder(os.Stdout).Encode(c)
			}

			_, _ = fmt.Fprintf(os.Stdout, "commit %s\n", c.CommitID)
			_, _ = fmt.Fprintf(os.Stdout, "Author: %s\n", c.AgentID)
			_, _ = fmt.Fprintf(os.Stdout, "Date:   %s\n", c.CreatedAt.Format(time.RFC3339))
			_, _ = fmt.Fprintf(os.Stdout, "Task:   %s\n", c.TaskID.String())
			_, _ = fmt.Fprintf(os.Stdout, "Type:   %s\n\n", c.CommitType)
			_, _ = fmt.Fprintf(os.Stdout, "    %s\n", c.Message)

			// A commit object does not store its FileDiffs, so compute them
			// against its parent (with content hunks) when absent, so `mgit show`
			// renders the real change — not just metadata. Refs: MGIT-33
			diffs := c.FileDiffs
			if len(diffs) == 0 && c.ParentID != "" {
				if computed, derr := app.Diff.DiffCommits(ctx, c.ParentID, c.CommitID); derr == nil {
					diffs = computed
				}
			}
			if len(diffs) > 0 {
				_, _ = fmt.Fprintln(os.Stdout)
				if stat {
					_, _ = fmt.Fprint(os.Stdout, app.Diff.FormatStat(diffs))
				} else {
					_, _ = fmt.Fprint(os.Stdout, app.Diff.FormatUnified(diffs))
				}
			}
			return nil
		},
	}

	cmd.Flags().StringVar(&taskID, "task-id", "", "Show task info")
	cmd.Flags().BoolVar(&formatJSON, "json", false, "Output as JSON")
	cmd.Flags().BoolVar(&stat, "stat", false, "Show file statistics only")
	cmd.Flags().StringVar(&format, "format", "", "Output format: json")
	return cmd
}
