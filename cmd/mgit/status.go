package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"

	"github.com/spf13/cobra"

	gitstore "github.com/hyper-swe/mgit/internal/store/git"
)

// statusCmd implements mgit status. Refs: FR-8.6, MGIT-4.1.5
func statusCmd() *cobra.Command {
	var taskID string
	var formatJSON, short, porcelain bool

	cmd := &cobra.Command{
		Use:   "status",
		Short: "Show working tree status",
		RunE: func(_ *cobra.Command, _ []string) error {
			app, err := openAppFromCwd()
			if err != nil {
				return err
			}
			defer app.Close()

			ctx := context.Background()
			// Auto-housekeep: keep the .mgit base coherent with the current local
			// working state before reading status (ADR-008 §3). A no-op on the
			// cheap path; fails loud rather than report a known-stale base.
			if err := app.Sync.EnsureSynced(ctx); err != nil {
				return fmt.Errorf("status: %w", err)
			}
			ws := gitstore.NewWorktreeStore(app.Repo)
			files, err := ws.Status(ctx)
			if err != nil {
				return fmt.Errorf("status: %w", err)
			}

			// Filter by task: only show files that are staged (task
			// scope is advisory — staging area is the filter proxy).
			if taskID != "" {
				_ = taskID // accepted for compatibility; worktree task scope
			}

			// Show current branch header.
			branch, branchErr := app.Repo.CurrentBranch()
			if branchErr != nil {
				branch = "HEAD (detached)"
			}

			if formatJSON {
				type statusJSON struct {
					Branch string                `json:"branch"`
					Files  []gitstore.FileStatus `json:"files"`
				}
				return json.NewEncoder(os.Stdout).Encode(statusJSON{
					Branch: branch,
					Files:  files,
				})
			}

			if porcelain {
				for _, f := range files {
					_, _ = fmt.Fprintf(os.Stdout, "%s%s %s\n", f.Staging, f.Worktree, f.Path)
				}
				return nil
			}

			if !short {
				_, _ = fmt.Fprintf(os.Stdout, "On branch %s\n", branch)
			}

			if len(files) == 0 {
				if !short {
					_, _ = fmt.Fprintln(os.Stdout, "nothing to commit, working tree clean")
				}
				return nil
			}

			if short {
				for _, f := range files {
					_, _ = fmt.Fprintf(os.Stdout, "%s%s %s\n", f.Staging, f.Worktree, f.Path)
				}
				return nil
			}
			printGroupedStatus(os.Stdout, files)
			return nil
		},
	}

	bindTaskIDFlag(cmd, &taskID, "Filter by task scope")
	cmd.Flags().BoolVar(&formatJSON, "json", false, "Output as JSON")
	cmd.Flags().BoolVar(&short, "short", false, "Compact status output")
	cmd.Flags().BoolVar(&porcelain, "porcelain", false, "Machine-readable output")
	return cmd
}

// printGroupedStatus renders the human status: staged, unstaged and untracked
// paths under headings that SAY which group will be recorded by the next
// commit. Previously the only difference between "will be committed" and "will
// be silently dropped" was which column the letter sat in — unreadable to a
// person and invisible to an agent (MGIT-77). The machine-readable modes
// (--short/--porcelain/--json) keep the two-column form and are unaffected.
// Refs: FR-8.6, MGIT-77
func printGroupedStatus(w io.Writer, files []gitstore.FileStatus) {
	var staged, unstaged, untracked []gitstore.FileStatus
	for _, f := range files {
		switch {
		case f.Staging != gitstore.StatusUnmodified:
			staged = append(staged, f)
		case f.Worktree == gitstore.StatusUntracked:
			untracked = append(untracked, f)
		default:
			unstaged = append(unstaged, f)
		}
	}

	printStatusGroup(w, "Changes to be committed (these WILL be recorded):", staged, true)
	printStatusGroup(w, "Changes not staged for commit (these will NOT be recorded):", unstaged, false)
	printStatusGroup(w, "Untracked files (these will NOT be recorded):", untracked, false)

	if len(staged) == 0 {
		_, _ = fmt.Fprintf(w, "\n%d change(s) present but nothing staged — `mgit commit` would record "+
			"nothing and will refuse.\n", len(unstaged)+len(untracked))
	} else {
		_, _ = fmt.Fprintf(w, "\n%d staged, %d not staged.\n", len(staged), len(unstaged)+len(untracked))
	}
	if len(unstaged)+len(untracked) > 0 {
		_, _ = fmt.Fprintln(w, "  Stage with `mgit add <path>`, or commit everything with `mgit commit -a -m \"...\"`.")
	}
}

// printStatusGroup prints one titled group, or nothing when it is empty. The
// staged group's worktree column is always unmodified, so each entry is
// labeled from whichever column carries its code.
func printStatusGroup(w io.Writer, title string, files []gitstore.FileStatus, staged bool) {
	if len(files) == 0 {
		return
	}
	_, _ = fmt.Fprintf(w, "\n%s\n", title)
	for _, f := range files {
		code := f.Worktree
		if staged {
			code = f.Staging
		}
		_, _ = fmt.Fprintf(w, "\t%s%s\n", statusLabel(code), f.Path)
	}
}

// statusLabel turns a status code into a padded word. An unrecognized code
// falls back to the raw character rather than silently claiming a change type.
func statusLabel(code string) string {
	switch code {
	case gitstore.StatusAdded:
		return "new file:   "
	case gitstore.StatusDeleted:
		return "deleted:    "
	case gitstore.StatusUntracked:
		return ""
	case gitstore.StatusModified:
		return "modified:   "
	default:
		return code + ":          "
	}
}
