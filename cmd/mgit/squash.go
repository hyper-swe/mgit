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
	var taskID, message, messageFile, toGitOutput string
	var dryRun, formatJSON, toGit, toMain, apply bool

	cmd := &cobra.Command{
		Use:   "squash",
		Short: "Squash micro-commits for a task",
		Long: "Consolidate a task's micro-commits into one commit on its own " +
			"task/<ID> branch.\n\n" +
			"The message comes from -m/--message inline, or from --file/-F, which " +
			"reads it verbatim from a file (or from stdin when the path is -) with " +
			"no shell involved. The two are mutually exclusive. A message you supply " +
			"is recorded exactly as given — it is the message a reviewer reads in " +
			"the user's own git after --to-git, so nothing is appended to it. With " +
			"no message, mgit generates one summarizing the micro-commits.",
		RunE: func(cmd *cobra.Command, _ []string) error {
			// Past flag parsing, a failure is a runtime condition, not misuse:
			// dumping the flag table after it buries the remedy. Refs: MGIT-77
			cmd.SilenceUsage = true

			// Resolve the message BEFORE the repository is opened or any commit,
			// branch or patch is written: an unreadable message file must squash
			// nothing. Refs: MGIT-105, MGIT-106
			resolved, err := resolveMessage(cmd, "squash", message, messageFile)
			if err != nil {
				return err
			}
			message = resolved

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

			if toMain && !dryRun {
				if err := promoteSquashToMain(ctx, app, squashed.Branch); err != nil {
					return err
				}
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

			printSquashResult(squashed, dryRun, toMain)
			return nil
		},
	}

	bindTaskIDFlag(cmd, &taskID, "Task to squash (required)")
	bindMessageFlags(cmd, &message, &messageFile, "squash",
		"Custom squash message (auto-generated from the micro-commits if empty)")
	cmd.Flags().BoolVar(&dryRun, "dry-run", false, "Preview without making changes")
	cmd.Flags().BoolVar(&formatJSON, "json", false, "Output as JSON")
	cmd.Flags().BoolVar(&toGit, "to-git", false, "Export squashed commit as git format-patch")
	cmd.Flags().StringVar(&toGitOutput, "to-git-output", "", "Write --to-git patch to file (default: stdout)")
	cmd.Flags().BoolVar(&toMain, "to-main", false, "Fast-forward merge squash commit to main branch")
	cmd.Flags().BoolVar(&apply, "apply", false, "Alias for --to-git that also writes the patch file")
	return cmd
}

// promoteSquashToMain integrates a task's squash into main (FR-7.2 step 5).
// The squash lives on its own task branch parented off the task base, so this
// fast-forwards main when possible or creates a merge commit otherwise — main
// is genuinely advanced, not just checked out. Refs: FR-7.2, MGIT-22
func promoteSquashToMain(ctx context.Context, app *App, sourceBranch string) error {
	if err := app.Branch.SwitchBranch(ctx, "main"); err != nil {
		return fmt.Errorf("squash --to-main: switch to main: %w", err)
	}
	res, err := app.Merge.Merge(ctx, service.MergeRequest{
		SourceBranch: sourceBranch,
		Strategy:     service.MergeAuto,
	})
	if err != nil {
		return fmt.Errorf("squash --to-main: %w", err)
	}
	_, _ = fmt.Fprintf(os.Stdout, "Promoted squash to main (%s): %s\n", res.Status, res.MergedHash)
	return nil
}

// printSquashResult reports what the squash did, in the human (non-JSON) form.
//
// The trailing note makes the resulting branch state unambiguous (MGIT-22): the
// squash lands on its own task branch; main is untouched and the originals are
// retained until the user promotes (--to-main) or exports (--to-git). It is
// suppressed when --to-main already promoted it. Refs: FR-7, MGIT-22
func printSquashResult(squashed *model.Commit, dryRun, promoted bool) {
	if dryRun {
		_, _ = fmt.Fprintf(os.Stdout, "[dry-run] Would create squash commit:\n%s\n", squashed.Message)
		return
	}
	_, _ = fmt.Fprintf(os.Stdout, "[%s] %s\n", squashed.ShortID(), squashed.Message)
	if !promoted {
		_, _ = fmt.Fprintf(os.Stdout,
			"squashed onto %s (main unchanged; --to-main to promote, --to-git to export)\n",
			squashed.Branch)
	}
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
