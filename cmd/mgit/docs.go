package main

import (
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/spf13/cobra"

	"github.com/hyper-swe/mgit/internal/docs"
)

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
