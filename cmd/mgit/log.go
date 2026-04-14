package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/hyper-swe/mgit-dev/internal/model"
)

// logCmd implements mgit log. Refs: FR-8.4, MGIT-4.1.3
func logCmd() *cobra.Command {
	var taskID, since, until, author, format string
	var limit int
	var formatJSON, oneline, graph bool

	cmd := &cobra.Command{
		Use:   "log",
		Short: "Show commit history",
		RunE: func(_ *cobra.Command, _ []string) error {
			app, err := openAppFromCwd()
			if err != nil {
				return err
			}
			defer app.Close()

			ctx := context.Background()

			// Task-filtered log
			if taskID != "" {
				records, err := app.Commit.GetTaskCommits(ctx, taskID)
				if err != nil {
					return fmt.Errorf("log: %w", err)
				}
				if formatJSON {
					return json.NewEncoder(os.Stdout).Encode(records)
				}
				for _, r := range records {
					_, _ = fmt.Fprintf(os.Stdout, "%s [%s] pos=%d\n", r.CommitHash[:8], r.TaskID, r.Position)
				}
				return nil
			}

			commits, err := app.Commit.ListCommits(ctx)
			if err != nil {
				return fmt.Errorf("log: %w", err)
			}

			// Apply --since / --until / --author filters
			commits = filterCommits(commits, since, until, author)

			if formatJSON || format == "json" {
				if limit > 0 && limit < len(commits) {
					commits = commits[:limit]
				}
				return json.NewEncoder(os.Stdout).Encode(commits)
			}

			return renderLog(commits, limit, oneline, graph, format)
		},
	}

	cmd.Flags().StringVar(&taskID, "task-id", "", "Filter by task ID")
	cmd.Flags().IntVarP(&limit, "limit", "n", 20, "Maximum commits to show")
	cmd.Flags().BoolVar(&formatJSON, "json", false, "Output as JSON")
	cmd.Flags().BoolVar(&oneline, "oneline", false, "One-line compact format")
	cmd.Flags().BoolVar(&graph, "graph", false, "Show commit graph")
	cmd.Flags().StringVar(&since, "since", "", "Show commits after date (RFC3339)")
	cmd.Flags().StringVar(&until, "until", "", "Show commits before date (RFC3339)")
	cmd.Flags().StringVar(&author, "author", "", "Filter by author/agent ID")
	cmd.Flags().StringVar(&format, "format", "", "Output format: oneline | full | json")
	return cmd
}

// filterCommits applies --since, --until, --author filters to a commit list.
// Refs: FR-8.4
func filterCommits(commits []*model.Commit, since, until, author string) []*model.Commit {
	if since == "" && until == "" && author == "" {
		return commits
	}
	var sinceT, untilT time.Time
	if since != "" {
		if t, err := time.Parse(time.RFC3339, since); err == nil {
			sinceT = t
		}
	}
	if until != "" {
		if t, err := time.Parse(time.RFC3339, until); err == nil {
			untilT = t
		}
	}
	var out []*model.Commit
	for _, c := range commits {
		if !sinceT.IsZero() && c.CreatedAt.Before(sinceT) {
			continue
		}
		if !untilT.IsZero() && c.CreatedAt.After(untilT) {
			continue
		}
		if author != "" && c.AgentID != author {
			continue
		}
		out = append(out, c)
	}
	return out
}

// renderLog writes commits to stdout in the requested format.
// Refs: FR-8.4
func renderLog(commits []*model.Commit, limit int, oneline, graph bool, format string) error {
	shown := 0
	for _, c := range commits {
		if limit > 0 && shown >= limit {
			break
		}
		prefix := ""
		if graph {
			prefix = "* "
		}
		switch {
		case oneline || format == "oneline":
			_, _ = fmt.Fprintf(os.Stdout, "%s%s %s\n", prefix, c.ShortID(), firstLine(c.Message))
		case format == "full":
			_, _ = fmt.Fprintf(os.Stdout, "%scommit %s\n", prefix, c.CommitID)
			_, _ = fmt.Fprintf(os.Stdout, "Author: %s\n", c.AgentID)
			_, _ = fmt.Fprintf(os.Stdout, "Date:   %s\n", c.CreatedAt.UTC().Format(time.RFC3339))
			_, _ = fmt.Fprintf(os.Stdout, "Task:   %s\n", c.TaskID.String())
			_, _ = fmt.Fprintf(os.Stdout, "\n    %s\n\n", c.Message)
		default:
			_, _ = fmt.Fprintf(os.Stdout, "%s%s %s\n", prefix, c.ShortID(), c.Message)
		}
		shown++
	}
	return nil
}

// firstLine returns the first line of a multi-line string.
func firstLine(s string) string {
	if i := strings.Index(s, "\n"); i >= 0 {
		return s[:i]
	}
	return s
}
