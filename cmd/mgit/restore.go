package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"

	"github.com/spf13/cobra"
)

// restoreCmd implements mgit restore: a single file from a commit, or with
// --all the ENTIRE working tree back to a checkpoint commit's state (MGIT-55,
// the whole-tree checkpoint-recovery primitive). Refs: FR-6.7, MGIT-4.2.8, MGIT-55
func restoreCmd() *cobra.Command {
	var commitHash string
	var formatJSON, all, force bool

	cmd := &cobra.Command{
		Use:   "restore [file] [commit]",
		Short: "Restore a file — or with --all the whole working tree — from a commit",
		Args:  cobra.RangeArgs(0, 2),
		RunE: func(_ *cobra.Command, args []string) error {
			// Resolve the source commit. Per-file form: `restore <file> <commit>`
			// or `restore <file> --commit <hash>`. Whole-tree form:
			// `restore --all <commit>` or `restore --all --commit <hash>`.
			if all {
				if len(args) > 1 {
					return fmt.Errorf("restore --all takes no file argument (it restores the whole tree)")
				}
				commitHash = firstNonEmpty(argAt(args, 0), commitHash)
			} else {
				commitHash = firstNonEmpty(argAt(args, 1), commitHash)
			}
			if commitHash == "" {
				return fmt.Errorf("a source commit (positional or --commit) is required")
			}

			app, err := openAppFromCwd()
			if err != nil {
				return err
			}
			defer app.Close()

			ctx := context.Background()

			// --all: whole-tree checkpoint recovery (working dir only; review
			// and commit the recovered state as the next step).
			if all {
				res, err := app.Restore.RestoreAll(ctx, commitHash, force)
				if err != nil {
					return err
				}
				if formatJSON {
					return json.NewEncoder(os.Stdout).Encode(res)
				}
				short := res.CommitHash
				if len(short) > 8 {
					short = short[:8]
				}
				if res.Status == "unchanged" {
					_, _ = fmt.Fprintf(os.Stdout, "Already at checkpoint %s (nothing to restore)\n", short)
					return nil
				}
				_, _ = fmt.Fprintf(os.Stdout, "Restored working tree to checkpoint %s (%d files changed, staged); review and commit\n",
					short, res.FilesChanged)
				return nil
			}

			if len(args) == 0 {
				return fmt.Errorf("a file argument is required (or pass --all for the whole tree)")
			}
			result, err := app.Restore.RestoreFile(ctx, args[0], commitHash)
			if err != nil {
				return fmt.Errorf("restore: %w", err)
			}

			if formatJSON {
				return json.NewEncoder(os.Stdout).Encode(result)
			}
			short := commitHash
			if len(short) > 8 {
				short = short[:8]
			}
			_, _ = fmt.Fprintf(os.Stdout, "Restored %s (%d bytes) from commit %s\n",
				result.Path, result.BytesWrit, short)
			return nil
		},
	}

	cmd.Flags().StringVar(&commitHash, "commit", "", "Commit to restore from (required)")
	cmd.Flags().BoolVar(&all, "all", false, "Restore the entire working tree to the commit's state (restored paths are staged; no commit is created)")
	cmd.Flags().BoolVar(&force, "force", false, "With --all: restore over uncommitted local changes (checkpoint recovery)")
	cmd.Flags().BoolVar(&formatJSON, "json", false, "Output as JSON")
	return cmd
}
