package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"

	"github.com/spf13/cobra"

	"github.com/hyper-swe/mgit/internal/model"
	"github.com/hyper-swe/mgit/internal/service"
)

// squashCmd implements mgit squash. Refs: FR-8.11, MGIT-4.2.2
func squashCmd() *cobra.Command {
	var taskID, message, toGitOutput string
	var dryRun, formatJSON, toGit, toMain, apply bool

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

			// --to-main switches the shared HEAD to main; from a linked worktree
			// that would corrupt the parent's HEAD (and the merge would target the
			// bound branch, not main). Promote from the parent. Refs: MGIT-24
			if toMain && app.BoundTask != "" {
				return fmt.Errorf("cannot --to-main from a linked worktree (bound to task %s); run it from the parent repository", app.BoundTask)
			}

			ctx := context.Background()

			// --apply implies --to-git behavior.
			if apply {
				toGit = true
			}

			squashed, err := app.Squash.SquashTask(ctx, service.SquashRequest{
				TaskID:  taskID,
				Message: message,
				DryRun:  dryRun,
			})
			if err != nil {
				return fmt.Errorf("squash: %w", err)
			}

			// --to-main: integrate the task's squash into main (FR-7.2 step 5).
			// The squash lives on its own task branch parented off the task base,
			// so this fast-forwards main when possible or creates a merge commit
			// otherwise — main is genuinely advanced, not just checked out.
			if toMain && !dryRun {
				if err := app.Branch.SwitchBranch(ctx, "main"); err != nil {
					return fmt.Errorf("squash --to-main: switch to main: %w", err)
				}
				res, err := app.Merge.Merge(ctx, service.MergeRequest{
					SourceBranch: squashed.Branch,
					Strategy:     service.MergeAuto,
				})
				if err != nil {
					return fmt.Errorf("squash --to-main: %w", err)
				}
				_, _ = fmt.Fprintf(os.Stdout, "Promoted squash to main (%s): %s\n", res.Status, res.MergedHash)
			}

			if toGit {
				return emitSquashPatch(ctx, app, squashPatchOptions{
					taskID:   taskID,
					message:  message,
					dryRun:   dryRun,
					squashed: squashed,
					outPath:  toGitOutput,
				})
			}

			if formatJSON {
				return json.NewEncoder(os.Stdout).Encode(squashed)
			}

			if dryRun {
				_, _ = fmt.Fprintf(os.Stdout, "[dry-run] Would create squash commit:\n%s\n", squashed.Message)
			} else {
				_, _ = fmt.Fprintf(os.Stdout, "[%s] %s\n", squashed.ShortID(), squashed.Message)
				// Make the resulting branch state unambiguous (MGIT-22): the squash
				// lands on its own task branch; main is untouched and the originals
				// are retained until the user promotes (--to-main) or exports
				// (--to-git). Suppressed when --to-main already promoted it.
				if !toMain {
					_, _ = fmt.Fprintf(os.Stdout,
						"squashed onto %s (main unchanged; --to-main to promote, --to-git to export)\n",
						squashed.Branch)
				}
			}
			return nil
		},
	}

	bindTaskIDFlag(cmd, &taskID, "Task to squash (required)")
	cmd.Flags().StringVar(&message, "message", "", "Custom squash message")
	cmd.Flags().BoolVar(&dryRun, "dry-run", false, "Preview without making changes")
	cmd.Flags().BoolVar(&formatJSON, "json", false, "Output as JSON")
	cmd.Flags().BoolVar(&toGit, "to-git", false, "Export squashed commit as git format-patch")
	cmd.Flags().StringVar(&toGitOutput, "to-git-output", "", "Write --to-git patch to file (default: stdout)")
	cmd.Flags().BoolVar(&toMain, "to-main", false, "Fast-forward merge squash commit to main branch")
	cmd.Flags().BoolVar(&apply, "apply", false, "Alias for --to-git that also writes the patch file")
	return cmd
}

// squashPatchOptions carries the inputs to --to-git. squashed is the commit
// SquashTask produced; under --dry-run it has no identity, so the patch comes
// from the read-only preview instead. Refs: FR-7, MGIT-112
type squashPatchOptions struct {
	taskID   string
	message  string
	dryRun   bool
	squashed *model.Commit
	outPath  string
}

// emitSquashPatch renders the task's git format-patch and writes it to stdout
// or --to-git-output.
//
// Under --dry-run no squash commit exists to diff, so it renders through
// SquashService.PreviewGitPatch — the same read-only tree diff behind
// `mgit export --format git`. Before MGIT-112 that combination simply failed
// ("to commit is empty"); the preview path makes --dry-run --to-git a real
// preview, and reports a genuinely empty net change instead of emitting a
// hunk-free patch. Refs: FR-7, MGIT-112, MGIT-77
func emitSquashPatch(ctx context.Context, app *App, opts squashPatchOptions) error {
	patch, err := squashPatchText(ctx, app, opts)
	if err != nil {
		return fmt.Errorf("squash --to-git: %w", err)
	}
	// A genuinely empty net change was already reported on stderr; emitting an
	// empty patch file or an empty mbox is the silent-loss shape. Refs: MGIT-112
	if patch == "" {
		return nil
	}
	// A hunk-free patch is well formed and `git apply --allow-empty` accepts
	// it, so the land step would report success and land nothing. Say so on
	// stderr — stdout stays a clean patch when piped. Refs: MGIT-77
	if !service.PatchHasHunks(patch) {
		_, _ = fmt.Fprintf(os.Stderr,
			"warning: the patch for task %s contains NO diff hunks — "+
				"applying it will change nothing.\n"+
				"  Its commits recorded no content. Check `mgit log --oneline` and "+
				"`mgit diff --task-id %s`; work is recorded only when staged "+
				"(`mgit add <path>`, or `mgit commit -a`).\n",
			opts.taskID, opts.taskID)
	}
	if opts.outPath != "" {
		if err := os.WriteFile(opts.outPath, []byte(patch), 0o600); err != nil {
			return fmt.Errorf("squash --to-git: write patch: %w", err)
		}
		_, _ = fmt.Fprintf(os.Stdout, "Wrote git patch to %s\n", opts.outPath)
		return nil
	}
	_, _ = fmt.Fprint(os.Stdout, patch)
	return nil
}

// squashPatchText returns the patch to emit, or "" when the task's net change
// is genuinely empty (having printed the explanatory note to stderr).
// Refs: MGIT-112
func squashPatchText(ctx context.Context, app *App, opts squashPatchOptions) (string, error) {
	if !opts.dryRun {
		return app.Squash.GitFormatPatch(ctx, opts.squashed)
	}
	preview, err := app.Squash.PreviewGitPatch(ctx, service.SquashRequest{
		TaskID:  opts.taskID,
		Message: opts.message,
	})
	if err != nil {
		return "", err
	}
	if preview.Empty {
		_, _ = fmt.Fprintln(os.Stderr, emptyNetChangeNote(opts.taskID))
		return "", nil
	}
	return preview.Patch, nil
}
