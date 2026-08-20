package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"

	"github.com/spf13/cobra"

	"github.com/hyper-swe/mgit/internal/service"
)

// mergeCmd implements mgit merge. Refs: FR-8.4, MGIT-4.2.10
func mergeCmd() *cobra.Command {
	var squash, noFF, formatJSON bool
	var message, messageFile string

	cmd := &cobra.Command{
		Use:   "merge [branch]",
		Short: "Merge a branch into the current branch",
		Long: "Merge a branch into the current branch.\n\n" +
			"The merge commit's message comes from -m/--message inline, or from " +
			"--file/-F, which reads it verbatim from a file (or from stdin when the " +
			"path is -) with no shell involved. The two are mutually exclusive.",
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			// Past flag parsing, a failure is a runtime condition, not misuse:
			// dumping the flag table after it buries the remedy. Refs: MGIT-77
			cmd.SilenceUsage = true

			// Resolve the message BEFORE the repository is opened or the merge
			// commit is written: an unreadable message file must merge nothing.
			// Refs: MGIT-105, MGIT-106
			resolved, err := resolveMessage(cmd, "merge", message, messageFile)
			if err != nil {
				return err
			}
			message = resolved

			app, err := openAppFromCwd()
			if err != nil {
				return err
			}
			defer app.Close()

			ctx := context.Background()
			strategy := service.MergeAuto
			switch {
			case squash && noFF:
				return fmt.Errorf("merge: --squash and --no-ff are mutually exclusive")
			case squash:
				strategy = service.MergeSquash
			case noFF:
				strategy = service.MergeNoFF
			}

			result, err := app.Merge.Merge(ctx, service.MergeRequest{
				SourceBranch: args[0],
				Strategy:     strategy,
				Message:      message,
			})
			if err != nil {
				return err
			}

			if formatJSON {
				return json.NewEncoder(os.Stdout).Encode(result)
			}
			short := result.MergedHash
			if len(short) > 8 {
				short = short[:8]
			}
			_, _ = fmt.Fprintf(os.Stdout, "[%s] %s %s into %s\n",
				short, result.Status, result.Source, result.Target)
			return nil
		},
	}

	cmd.Flags().BoolVar(&squash, "squash", false, "Squash merge into a single commit")
	cmd.Flags().BoolVar(&noFF, "no-ff", false, "Always create a merge commit (no fast-forward)")
	bindMessageFlags(cmd, &message, &messageFile, "merge",
		"Custom merge commit message (auto-generated if empty)")
	cmd.Flags().BoolVar(&formatJSON, "json", false, "Output as JSON")
	return cmd
}
