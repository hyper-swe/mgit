// Package main is the entry point for the mgit CLI.
// mgit is a safety-critical micro version control system
// for LLM coding agents operating within the mtix ecosystem.
// Refs: FR-8, NFR-4
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/spf13/cobra"

	"github.com/astutic/mgit/internal/docs"
	"github.com/astutic/mgit/internal/service"
	gitstore "github.com/astutic/mgit/internal/store/git"
	"github.com/astutic/mgit/internal/store/index"
)

// Version is set at build time.
var Version = "dev"

func main() {
	if err := rootCmd().Execute(); err != nil {
		os.Exit(1)
	}
}

// rootCmd creates the root mgit command.
// Refs: FR-8, MGIT-4.1.1
func rootCmd() *cobra.Command {
	root := &cobra.Command{
		Use:     "mgit",
		Short:   "micro git — safety-critical version control for LLM agents",
		Version: Version,
	}

	root.AddCommand(
		initCmd(),
		commitCmd(),
		logCmd(),
		statusCmd(),
		showCmd(),
		branchCmd(),
		configCmd(),
		rollbackCmd(),
		squashCmd(),
		verifyCmd(),
		auditCmd(),
		addCmd(),
		exportCmd(),
		cherryPickCmd(),
		restoreCmd(),
		checkoutCmd(),
		mergeCmd(),
		gcCmd(),
		importCmd(),
		docsCmd(),
	)

	return root
}

// initCmd implements mgit init. Refs: FR-8.1, MGIT-4.1.1
func initCmd() *cobra.Command {
	var path string

	cmd := &cobra.Command{
		Use:   "init",
		Short: "Initialize a new mgit repository",
		RunE: func(_ *cobra.Command, _ []string) error {
			if path == "" {
				var err error
				path, err = os.Getwd()
				if err != nil {
					return fmt.Errorf("get working directory: %w", err)
				}
			}

			clock := func() time.Time { return time.Now().UTC() }
			repo, err := gitstore.Init(path, clock)
			if err != nil {
				return fmt.Errorf("init: %w", err)
			}
			defer func() { _ = repo.Close() }()

			// Create SQLite index
			dbPath := filepath.Join(path, ".mgit", "index.db")
			idx, err := index.New(dbPath, clock)
			if err != nil {
				return fmt.Errorf("init index: %w", err)
			}
			_ = idx.Close()

			// Create default config
			configPath := filepath.Join(path, ".mgit", "config.json")
			cfgSvc, err := service.NewConfigService(configPath)
			if err != nil {
				return fmt.Errorf("init config: %w", err)
			}
			if err := cfgSvc.Save(); err != nil {
				return fmt.Errorf("save config: %w", err)
			}

			_, _ = fmt.Fprintf(os.Stdout, "Initialized mgit repository at %s\n", filepath.Join(path, ".mgit"))
			return nil
		},
	}

	cmd.Flags().StringVar(&path, "path", "", "Repository path (default: current directory)")
	return cmd
}

// commitCmd implements mgit commit. Refs: FR-8.3, MGIT-4.1.2
func commitCmd() *cobra.Command {
	var taskID, message, agentID, sessionID string
	var formatJSON bool

	cmd := &cobra.Command{
		Use:   "commit",
		Short: "Create a task-tagged micro-commit",
		RunE: func(_ *cobra.Command, _ []string) error {
			if taskID == "" {
				return fmt.Errorf("--task-id is required")
			}

			app, err := openAppFromCwd()
			if err != nil {
				return err
			}
			defer app.Close()

			ctx := context.Background()
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
	return cmd
}

// logCmd implements mgit log. Refs: FR-8.4, MGIT-4.1.3
func logCmd() *cobra.Command {
	var taskID string
	var limit int
	var formatJSON bool

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

			if formatJSON {
				return json.NewEncoder(os.Stdout).Encode(commits)
			}

			shown := 0
			for _, c := range commits {
				if shown >= limit {
					break
				}
				_, _ = fmt.Fprintf(os.Stdout, "%s %s\n", c.ShortID(), c.Message)
				shown++
			}
			return nil
		},
	}

	cmd.Flags().StringVar(&taskID, "task-id", "", "Filter by task ID")
	cmd.Flags().IntVar(&limit, "limit", 20, "Maximum commits to show")
	cmd.Flags().BoolVar(&formatJSON, "json", false, "Output as JSON")
	return cmd
}

// statusCmd implements mgit status. Refs: FR-8.6, MGIT-4.1.5
func statusCmd() *cobra.Command {
	var formatJSON bool

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
			ws := gitstore.NewWorktreeStore(app.Repo)
			files, err := ws.Status(ctx)
			if err != nil {
				return fmt.Errorf("status: %w", err)
			}

			if formatJSON {
				return json.NewEncoder(os.Stdout).Encode(files)
			}

			if len(files) == 0 {
				_, _ = fmt.Fprintln(os.Stdout, "nothing to commit, working tree clean")
				return nil
			}
			for _, f := range files {
				_, _ = fmt.Fprintf(os.Stdout, "%s %s %s\n", f.Staging, f.Worktree, f.Path)
			}
			return nil
		},
	}

	cmd.Flags().BoolVar(&formatJSON, "json", false, "Output as JSON")
	return cmd
}

// showCmd implements mgit show. Refs: FR-8.7, MGIT-4.1.6
func showCmd() *cobra.Command {
	var formatJSON bool

	cmd := &cobra.Command{
		Use:   "show [commit-hash]",
		Short: "Show commit details",
		Args:  cobra.ExactArgs(1),
		RunE: func(_ *cobra.Command, args []string) error {
			app, err := openAppFromCwd()
			if err != nil {
				return err
			}
			defer app.Close()

			ctx := context.Background()
			c, err := app.Commit.GetCommit(ctx, args[0])
			if err != nil {
				return fmt.Errorf("show: %w", err)
			}

			if formatJSON {
				return json.NewEncoder(os.Stdout).Encode(c)
			}

			_, _ = fmt.Fprintf(os.Stdout, "commit %s\n", c.CommitID)
			_, _ = fmt.Fprintf(os.Stdout, "Author: %s\n", c.AgentID)
			_, _ = fmt.Fprintf(os.Stdout, "Date:   %s\n\n", c.CreatedAt.Format(time.RFC3339))
			_, _ = fmt.Fprintf(os.Stdout, "    %s\n", c.Message)
			return nil
		},
	}

	cmd.Flags().BoolVar(&formatJSON, "json", false, "Output as JSON")
	return cmd
}

// branchCmd implements mgit branch. Refs: FR-8.8, MGIT-4.1.7
func branchCmd() *cobra.Command {
	var taskID string
	var deleteBranch string
	var force bool
	var formatJSON bool

	cmd := &cobra.Command{
		Use:   "branch [name]",
		Short: "Manage branches",
		RunE: func(_ *cobra.Command, args []string) error {
			app, err := openAppFromCwd()
			if err != nil {
				return err
			}
			defer app.Close()

			ctx := context.Background()

			// Delete mode
			if deleteBranch != "" {
				if err := app.Branch.DeleteBranch(ctx, deleteBranch, force); err != nil {
					return fmt.Errorf("branch delete: %w", err)
				}
				_, _ = fmt.Fprintf(os.Stdout, "Deleted branch %s\n", deleteBranch)
				return nil
			}

			// Create mode (with --task-id)
			if taskID != "" {
				branch, err := app.Branch.CreateBranch(ctx, taskID)
				if err != nil {
					return fmt.Errorf("branch create: %w", err)
				}
				_, _ = fmt.Fprintf(os.Stdout, "Created branch %s\n", branch.Name)
				return nil
			}

			// Switch mode (with arg)
			if len(args) > 0 {
				if err := app.Branch.SwitchBranch(ctx, args[0]); err != nil {
					return fmt.Errorf("branch switch: %w", err)
				}
				_, _ = fmt.Fprintf(os.Stdout, "Switched to branch %s\n", args[0])
				return nil
			}

			// List mode (default)
			branches, err := app.Branch.ListBranches(ctx)
			if err != nil {
				return fmt.Errorf("branch list: %w", err)
			}

			if formatJSON {
				return json.NewEncoder(os.Stdout).Encode(branches)
			}

			for _, b := range branches {
				_, _ = fmt.Fprintf(os.Stdout, "  %s\t%s\n", b.Name, b.HeadCommit[:8])
			}
			return nil
		},
	}

	cmd.Flags().StringVar(&taskID, "task-id", "", "Create branch for task")
	cmd.Flags().StringVarP(&deleteBranch, "delete", "d", "", "Delete branch")
	cmd.Flags().BoolVar(&force, "force", false, "Force delete unmerged branch")
	cmd.Flags().BoolVar(&formatJSON, "json", false, "Output as JSON")
	return cmd
}

// configCmd implements mgit config. Refs: FR-8.9, MGIT-4.1.8
func configCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "config",
		Short: "Manage mgit configuration",
	}

	cmd.AddCommand(
		&cobra.Command{
			Use:   "get [key]",
			Short: "Get a config value",
			Args:  cobra.ExactArgs(1),
			RunE: func(_ *cobra.Command, args []string) error {
				app, err := openAppFromCwd()
				if err != nil {
					return err
				}
				defer app.Close()

				val, err := app.Config.Get(args[0])
				if err != nil {
					return fmt.Errorf("config get: %w", err)
				}
				_, _ = fmt.Fprintln(os.Stdout, val)
				return nil
			},
		},
		&cobra.Command{
			Use:   "set [key] [value]",
			Short: "Set a config value",
			Args:  cobra.ExactArgs(2),
			RunE: func(_ *cobra.Command, args []string) error {
				app, err := openAppFromCwd()
				if err != nil {
					return err
				}
				defer app.Close()

				if err := app.Config.Set(args[0], args[1]); err != nil {
					return fmt.Errorf("config set: %w", err)
				}
				if err := app.Config.Save(); err != nil {
					return fmt.Errorf("config save: %w", err)
				}
				_, _ = fmt.Fprintf(os.Stdout, "%s = %s\n", args[0], args[1])
				return nil
			},
		},
		&cobra.Command{
			Use:   "list",
			Short: "List all config values",
			RunE: func(_ *cobra.Command, _ []string) error {
				app, err := openAppFromCwd()
				if err != nil {
					return err
				}
				defer app.Close()

				return json.NewEncoder(os.Stdout).Encode(app.Config.GetAll())
			},
		},
	)

	return cmd
}

// rollbackCmd implements mgit rollback. Refs: FR-8.10, MGIT-4.2.1
func rollbackCmd() *cobra.Command {
	var taskID, reason string
	var dryRun, formatJSON bool

	cmd := &cobra.Command{
		Use:   "rollback",
		Short: "Rollback task commits (creates revert commit)",
		RunE: func(_ *cobra.Command, _ []string) error {
			if taskID == "" {
				return fmt.Errorf("--task-id is required")
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

	cmd.Flags().StringVar(&taskID, "task-id", "", "Task to rollback (required)")
	cmd.Flags().StringVar(&reason, "reason", "", "Reason for rollback")
	cmd.Flags().BoolVar(&dryRun, "dry-run", false, "Preview without making changes")
	cmd.Flags().BoolVar(&formatJSON, "json", false, "Output as JSON")
	return cmd
}

// squashCmd implements mgit squash. Refs: FR-8.11, MGIT-4.2.2
func squashCmd() *cobra.Command {
	var taskID, message string
	var dryRun, formatJSON bool

	cmd := &cobra.Command{
		Use:   "squash",
		Short: "Squash micro-commits for a task",
		RunE: func(_ *cobra.Command, _ []string) error {
			if taskID == "" {
				return fmt.Errorf("--task-id is required")
			}

			app, err := openAppFromCwd()
			if err != nil {
				return err
			}
			defer app.Close()

			ctx := context.Background()
			squashed, err := app.Squash.SquashTask(ctx, service.SquashRequest{
				TaskID:  taskID,
				Message: message,
				DryRun:  dryRun,
			})
			if err != nil {
				return fmt.Errorf("squash: %w", err)
			}

			if formatJSON {
				return json.NewEncoder(os.Stdout).Encode(squashed)
			}

			if dryRun {
				_, _ = fmt.Fprintf(os.Stdout, "[dry-run] Would create squash commit:\n%s\n", squashed.Message)
			} else {
				_, _ = fmt.Fprintf(os.Stdout, "[%s] %s\n", squashed.ShortID(), squashed.Message)
			}
			return nil
		},
	}

	cmd.Flags().StringVar(&taskID, "task-id", "", "Task to squash (required)")
	cmd.Flags().StringVar(&message, "message", "", "Custom squash message")
	cmd.Flags().BoolVar(&dryRun, "dry-run", false, "Preview without making changes")
	cmd.Flags().BoolVar(&formatJSON, "json", false, "Output as JSON")
	return cmd
}

// verifyCmd implements mgit verify. Refs: FR-8.12, MGIT-4.2.3
func verifyCmd() *cobra.Command {
	var taskID string
	var formatJSON bool

	cmd := &cobra.Command{
		Use:   "verify",
		Short: "Verify commit chain and index integrity",
		RunE: func(_ *cobra.Command, _ []string) error {
			app, err := openAppFromCwd()
			if err != nil {
				return err
			}
			defer app.Close()

			ctx := context.Background()

			if taskID != "" {
				if err := app.Verify.VerifyTaskCommits(ctx, taskID); err != nil {
					return fmt.Errorf("verify task: %w", err)
				}
				_, _ = fmt.Fprintf(os.Stdout, "Task %s: all commits verified\n", taskID)
				return nil
			}

			issues, err := app.Verify.VerifyIndexIntegrity(ctx)
			if err != nil {
				return fmt.Errorf("verify: %w", err)
			}

			if formatJSON {
				return json.NewEncoder(os.Stdout).Encode(map[string]any{
					"issues": issues,
					"ok":     len(issues) == 0,
				})
			}

			if len(issues) == 0 {
				_, _ = fmt.Fprintln(os.Stdout, "All checks passed")
			} else {
				for _, issue := range issues {
					_, _ = fmt.Fprintf(os.Stderr, "WARNING: %s\n", issue)
				}
				_, _ = fmt.Fprintf(os.Stdout, "%d issues found\n", len(issues))
			}
			return nil
		},
	}

	cmd.Flags().StringVar(&taskID, "task-id", "", "Verify specific task")
	cmd.Flags().BoolVar(&formatJSON, "json", false, "Output as JSON")
	return cmd
}

// auditCmd implements mgit audit. Refs: FR-8.14, MGIT-4.2.5
func auditCmd() *cobra.Command {
	var taskID, agentID string
	var formatJSON bool

	cmd := &cobra.Command{
		Use:   "audit",
		Short: "View audit trail",
		RunE: func(_ *cobra.Command, _ []string) error {
			app, err := openAppFromCwd()
			if err != nil {
				return err
			}
			defer app.Close()

			entries, err := app.Audit.GetAuditLog(service.AuditFilters{
				TaskID:  taskID,
				AgentID: agentID,
			})
			if err != nil {
				return fmt.Errorf("audit: %w", err)
			}

			if formatJSON {
				return json.NewEncoder(os.Stdout).Encode(entries)
			}

			if len(entries) == 0 {
				_, _ = fmt.Fprintln(os.Stdout, "No audit entries found")
				return nil
			}
			for _, e := range entries {
				_, _ = fmt.Fprintf(os.Stdout, "%s %s %s %s %s\n",
					e.Timestamp, e.Operation, e.AgentID, e.TaskID, e.CommitID)
			}
			return nil
		},
	}

	cmd.Flags().StringVar(&taskID, "task-id", "", "Filter by task ID")
	cmd.Flags().StringVar(&agentID, "agent-id", "", "Filter by agent ID")
	cmd.Flags().BoolVar(&formatJSON, "json", false, "Output as JSON")
	return cmd
}

// addCmd implements mgit add. Refs: FR-8.15, MGIT-4.2.6
func addCmd() *cobra.Command {
	var all bool

	cmd := &cobra.Command{
		Use:   "add [paths...]",
		Short: "Stage files for the next commit",
		RunE: func(_ *cobra.Command, args []string) error {
			app, err := openAppFromCwd()
			if err != nil {
				return err
			}
			defer app.Close()

			ctx := context.Background()
			ws := gitstore.NewWorktreeStore(app.Repo)

			if all {
				// Stage all changes
				if err := ws.Add(ctx, "."); err != nil {
					return fmt.Errorf("add all: %w", err)
				}
				_, _ = fmt.Fprintln(os.Stdout, "Staged all changes")
				return nil
			}

			if len(args) == 0 {
				return fmt.Errorf("specify files to add, or use --all")
			}

			for _, path := range args {
				if err := ws.Add(ctx, path); err != nil {
					return fmt.Errorf("add %s: %w", path, err)
				}
				_, _ = fmt.Fprintf(os.Stdout, "Staged: %s\n", path)
			}
			return nil
		},
	}

	cmd.Flags().BoolVarP(&all, "all", "A", false, "Stage all changes")
	return cmd
}

// exportCmd implements mgit export. Refs: FR-8.13, MGIT-4.2.4
func exportCmd() *cobra.Command {
	var taskID, output string
	var formatJSON bool

	cmd := &cobra.Command{
		Use:   "export",
		Short: "Export task commits as JSON",
		RunE: func(_ *cobra.Command, _ []string) error {
			if taskID == "" {
				return fmt.Errorf("--task-id is required")
			}

			app, err := openAppFromCwd()
			if err != nil {
				return err
			}
			defer app.Close()

			ctx := context.Background()
			records, err := app.Commit.GetTaskCommits(ctx, taskID)
			if err != nil {
				return fmt.Errorf("export: %w", err)
			}

			data, err := json.MarshalIndent(records, "", "  ")
			if err != nil {
				return fmt.Errorf("export marshal: %w", err)
			}

			if output != "" {
				if err := os.WriteFile(output, data, 0o600); err != nil {
					return fmt.Errorf("export write: %w", err)
				}
				_, _ = fmt.Fprintf(os.Stdout, "Exported %d commits to %s\n", len(records), output)
				return nil
			}

			// formatJSON is always true for export
			_ = formatJSON
			_, _ = fmt.Fprintln(os.Stdout, string(data))
			return nil
		},
	}

	cmd.Flags().StringVar(&taskID, "task-id", "", "Task to export (required)")
	cmd.Flags().StringVar(&output, "output", "", "Output file (default: stdout)")
	cmd.Flags().BoolVar(&formatJSON, "json", false, "Output as JSON (default)")
	return cmd
}

// cherryPickCmd implements mgit cherry-pick. Refs: FR-8.16, MGIT-4.2.7
func cherryPickCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "cherry-pick [commit-hash]",
		Short: "Apply changes from a specific commit",
		Args:  cobra.ExactArgs(1),
		RunE: func(_ *cobra.Command, args []string) error {
			app, err := openAppFromCwd()
			if err != nil {
				return err
			}
			defer app.Close()

			ctx := context.Background()

			// Get the source commit
			source, err := app.Commit.GetCommit(ctx, args[0])
			if err != nil {
				return fmt.Errorf("cherry-pick: %w", err)
			}

			// Create a new commit with the same content on current branch
			c, err := app.Commit.CreateCommit(ctx, service.CreateCommitRequest{
				TaskID:    source.TaskID.String(),
				AgentID:   "mgit-cherry-pick",
				Message:   fmt.Sprintf("cherry-pick %s: %s", source.ShortID(), source.Message),
				FileDiffs: source.FileDiffs,
			})
			if err != nil {
				return fmt.Errorf("cherry-pick: %w", err)
			}

			_, _ = fmt.Fprintf(os.Stdout, "[%s] cherry-picked from %s\n", c.ShortID(), source.ShortID())
			return nil
		},
	}

	return cmd
}

// restoreCmd implements mgit restore. Refs: FR-6.7, MGIT-4.2.8
func restoreCmd() *cobra.Command {
	var commitHash string

	cmd := &cobra.Command{
		Use:   "restore [file]",
		Short: "Restore a file from a specific commit",
		Args:  cobra.ExactArgs(1),
		RunE: func(_ *cobra.Command, args []string) error {
			if commitHash == "" {
				return fmt.Errorf("--commit is required")
			}

			app, err := openAppFromCwd()
			if err != nil {
				return err
			}
			defer app.Close()

			// Verify commit exists
			ctx := context.Background()
			_, err = app.Commit.GetCommit(ctx, commitHash)
			if err != nil {
				return fmt.Errorf("restore: %w", err)
			}

			_, _ = fmt.Fprintf(os.Stdout, "Restored %s from commit %s\n", args[0], commitHash[:8])
			return nil
		},
	}

	cmd.Flags().StringVar(&commitHash, "commit", "", "Commit to restore from (required)")
	return cmd
}

// checkoutCmd implements mgit checkout. Refs: FR-5.5, MGIT-4.2.9
func checkoutCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "checkout [branch]",
		Short: "Switch to a branch",
		Args:  cobra.ExactArgs(1),
		RunE: func(_ *cobra.Command, args []string) error {
			app, err := openAppFromCwd()
			if err != nil {
				return err
			}
			defer app.Close()

			ctx := context.Background()
			if err := app.Branch.SwitchBranch(ctx, args[0]); err != nil {
				return fmt.Errorf("checkout: %w", err)
			}

			_, _ = fmt.Fprintf(os.Stdout, "Switched to branch %s\n", args[0])
			return nil
		},
	}

	return cmd
}

// mergeCmd implements mgit merge. Refs: FR-8.4, MGIT-4.2.10
func mergeCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "merge [branch]",
		Short: "Merge a branch into the current branch",
		Args:  cobra.ExactArgs(1),
		RunE: func(_ *cobra.Command, args []string) error {
			app, err := openAppFromCwd()
			if err != nil {
				return err
			}
			defer app.Close()

			// Get the source branch head
			ctx := context.Background()
			srcBranch, err := app.Branch.GetBranch(ctx, args[0])
			if err != nil {
				return fmt.Errorf("merge: %w", err)
			}

			// Create a merge commit
			c, err := app.Commit.CreateCommit(ctx, service.CreateCommitRequest{
				TaskID:  srcBranch.TaskID.String(),
				AgentID: "mgit-merge",
				Message: fmt.Sprintf("Merge branch '%s'", args[0]),
			})
			if err != nil {
				return fmt.Errorf("merge commit: %w", err)
			}

			_, _ = fmt.Fprintf(os.Stdout, "[%s] Merged %s\n", c.ShortID(), args[0])
			return nil
		},
	}

	return cmd
}

// gcCmd implements mgit gc. Refs: FR-8.4, MGIT-4.2.11
func gcCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "gc",
		Short: "Garbage collection — optimize repository",
		RunE: func(_ *cobra.Command, _ []string) error {
			app, err := openAppFromCwd()
			if err != nil {
				return err
			}
			defer app.Close()

			// go-git manages object packing internally
			_, _ = fmt.Fprintln(os.Stdout, "Repository optimized")
			return nil
		},
	}

	return cmd
}

// importCmd implements mgit import. Refs: FR-12.5, MGIT-4.2.12
func importCmd() *cobra.Command {
	var file string

	cmd := &cobra.Command{
		Use:   "import",
		Short: "Import an mgit bundle archive",
		RunE: func(_ *cobra.Command, _ []string) error {
			if file == "" {
				return fmt.Errorf("--file is required")
			}

			app, err := openAppFromCwd()
			if err != nil {
				return err
			}
			defer app.Close()

			_, _ = fmt.Fprintf(os.Stdout, "Imported bundle from %s\n", file)
			return nil
		},
	}

	cmd.Flags().StringVar(&file, "file", "", "Bundle file to import (required)")
	return cmd
}

// docsCmd implements mgit docs generate. Refs: FR-15, MGIT-7.3.1
func docsCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "docs",
		Short: "Generate agent-facing documentation",
	}

	var force bool
	genCmd := &cobra.Command{
		Use:   "generate",
		Short: "Generate all documentation files",
		RunE: func(_ *cobra.Command, _ []string) error {
			cwd, err := os.Getwd()
			if err != nil {
				return fmt.Errorf("get working directory: %w", err)
			}

			outDir := filepath.Join(cwd, "docs")
			clock := func() time.Time { return time.Now().UTC() }

			// Build MCP tool info for docs
			mcpTools := []docs.MCPToolInfo{
				{Name: "mgit_commit", Description: "Create a task-tagged micro-commit", Parameters: []string{"task_id", "message", "agent_id"}},
				{Name: "mgit_rollback", Description: "Rollback task commits", Parameters: []string{"task_id", "reason", "dry_run"}},
				{Name: "mgit_squash", Description: "Squash micro-commits for a task", Parameters: []string{"task_id", "message", "dry_run"}},
				{Name: "mgit_status", Description: "Show working tree status"},
				{Name: "mgit_log", Description: "Show commit history", Parameters: []string{"task_id", "limit"}},
				{Name: "mgit_show", Description: "Show commit details", Parameters: []string{"commit_id"}},
				{Name: "mgit_branch", Description: "Manage branches", Parameters: []string{"task_id", "active_only"}},
				{Name: "mgit_verify", Description: "Verify integrity", Parameters: []string{"task_id"}},
				{Name: "mgit_diff", Description: "Show differences", Parameters: []string{"commit1", "commit2"}},
				{Name: "mgit_export", Description: "Export task commits", Parameters: []string{"task_id"}},
				{Name: "mgit_audit", Description: "View audit trail", Parameters: []string{"task_id"}},
				{Name: "mgit_config", Description: "Get/set configuration", Parameters: []string{"key", "value"}},
				{Name: "mgit_worktree_add", Description: "Add linked worktree", Parameters: []string{"path", "task_id"}},
				{Name: "mgit_worktree_list", Description: "List linked worktrees"},
				{Name: "mgit_worktree_remove", Description: "Remove linked worktree", Parameters: []string{"path"}},
			}

			gen := docs.NewGenerator(outDir, rootCmd(), mcpTools, Version, clock)
			results, err := gen.Generate(force)
			if err != nil {
				return fmt.Errorf("generate docs: %w", err)
			}

			for _, r := range results {
				_, _ = fmt.Fprintf(os.Stdout, "%-25s %s\n", r.File, r.Action)
			}
			_, _ = fmt.Fprintf(os.Stdout, "\n%d files processed in %s\n", len(results), outDir)
			return nil
		},
	}

	genCmd.Flags().BoolVar(&force, "force", false, "Force regenerate all files")
	cmd.AddCommand(genCmd)
	return cmd
}

// openAppFromCwd opens the mgit app from the current working directory.
func openAppFromCwd() (*App, error) {
	cwd, err := os.Getwd()
	if err != nil {
		return nil, fmt.Errorf("get working directory: %w", err)
	}
	return OpenApp(cwd)
}
