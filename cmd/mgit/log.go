package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/hyper-swe/mgit/internal/model"
)

// logCmd implements mgit log. Refs: FR-8.4, MGIT-4.1.3
func logCmd() *cobra.Command {
	var taskID, since, until, author, format string
	var limit int
	var formatJSON, oneline, graph bool

	cmd := &cobra.Command{
		Use:   "log",
		Short: "Show commit history",
		Long:  "Show commit history." + cadenceTokenDoc,
		RunE: func(_ *cobra.Command, _ []string) error {
			app, err := openAppFromCwd()
			if err != nil {
				return err
			}
			defer app.Close()

			ctx := context.Background()

			// Task-filtered log
			if taskID != "" {
				return runTaskLog(ctx, app, taskID, formatJSON)
			}

			commits, err := app.Commit.ListCommits(ctx)
			if err != nil {
				return fmt.Errorf("log: %w", err)
			}

			// Apply --since / --until / --author filters. An unparseable
			// window fails the command here rather than quietly returning the
			// whole trail (MGIT-172).
			commits, err = filterCommits(commits, since, until, author)
			if err != nil {
				return err
			}

			if formatJSON || format == "json" {
				if limit > 0 && limit < len(commits) {
					commits = commits[:limit]
				}
				return json.NewEncoder(os.Stdout).Encode(commits)
			}

			return renderLog(commits, limit, oneline, graph, format)
		},
	}

	bindTaskIDFlag(cmd, &taskID, "Filter by task ID")
	cmd.Flags().IntVarP(&limit, "limit", "n", 20, "Maximum commits to show")
	cmd.Flags().BoolVar(&formatJSON, "json", false, "Output as JSON")
	cmd.Flags().BoolVar(&oneline, "oneline", false, "One-line compact format")
	cmd.Flags().BoolVar(&graph, "graph", false, "Show commit graph")
	cmd.Flags().StringVar(&since, "since", "", "Show commits at or after this RFC 3339 timestamp, e.g. 2026-08-15T00:00:00Z")
	cmd.Flags().StringVar(&until, "until", "", "Show commits at or before this RFC 3339 timestamp, e.g. 2026-08-15T23:59:59Z")
	cmd.Flags().StringVar(&author, "author", "", "Filter by author/agent ID")
	cmd.Flags().StringVar(&format, "format", "", "Output format: oneline | full | json")
	return cmd
}

// filterCommits applies --since, --until, --author filters to a commit list.
//
// An unparseable window is REFUSED, never ignored. The output of a filter the
// tool could not apply is indistinguishable from one that matched everything
// — same commits, same order, exit 0 — and `mgit log` is the reviewer's view
// of the audit trail, so a trail the reader believes is narrowed is the
// failure (MGIT-172). Refusing beats guessing at other layouts: a second guess
// that is also wrong would be the same silent defect. Refs: FR-8.4, MGIT-172
func filterCommits(commits []*model.Commit, since, until, author string) ([]*model.Commit, error) {
	sinceT, err := parseWindowBound("--since", since)
	if err != nil {
		return nil, err
	}
	untilT, err := parseWindowBound("--until", until)
	if err != nil {
		return nil, err
	}
	if since == "" && until == "" && author == "" {
		return commits, nil
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
	return out, nil
}

// parseWindowBound parses one RFC 3339 window bound; an empty value is no
// bound. The refusal names the flag, the value and an acceptable form, so a
// reader who typed a plain date is told what to type instead. Refs: MGIT-172
func parseWindowBound(flag, value string) (time.Time, error) {
	if value == "" {
		return time.Time{}, nil
	}
	t, err := time.Parse(time.RFC3339, value)
	if err != nil {
		return time.Time{}, fmt.Errorf("log: %s %q is not an RFC 3339 timestamp; write it like %s — a date alone or a phrase "+
			"such as \"yesterday\" is refused rather than ignored, because a window the tool cannot apply would "+
			"otherwise return the whole trail as if it had matched", flag, value, "2006-01-02T15:04:05Z")
	}
	return t, nil
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
