package main

import (
	"context"
	"fmt"
	"io"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/spf13/cobra"

	"github.com/hyper-swe/mgit/internal/model"
	"github.com/hyper-swe/mgit/internal/sandboxd"
	"github.com/hyper-swe/mgit/internal/sandboxd/basecache"
	"github.com/hyper-swe/mgit/internal/sandboxd/baseprune"
	"github.com/hyper-swe/mgit/internal/sandboxd/daemonrec"
	"github.com/hyper-swe/mgit/internal/sandboxd/images"
)

// daemonAskTimeout bounds one daemon's answer to "what are you running?". A
// daemon that does not answer in time makes every verdict "cannot tell".
const daemonAskTimeout = 10 * time.Second

// hostPruneDeps wires prune to this machine: its base cache, each pinner's
// images.lock, and every live daemon's list of sandboxes.
//
// The cache is opened plainly, NOT through openBaseCache: that helper sweeps
// old staging trees, which is right before a compose and wrong here, where
// `--dry-run` promises to change nothing. Refs: MGIT-239
func hostPruneDeps() (baseprune.Deps, error) {
	cache, err := basecache.Open()
	if err != nil {
		return baseprune.Deps{}, err
	}
	return baseprune.Deps{
		Cache:    cache,
		LockPins: images.CachedPins,
		InUse:    func(ctx context.Context) (map[string]bool, error) { return liveSandboxDigests(ctx, listHostDaemons) },
	}, nil
}

// liveSandboxDigests asks every live daemon on this host which bases its
// sandboxes boot from.
//
// WHICH LAYER OWNS THIS FACT. A running VM is the daemon's, not the
// repository's: a repository's index can say "running" for a sandbox whose
// daemon died, and a repository nobody recorded as a pinner can still have a
// daemon running a sandbox on an entry. So the daemons are asked, all of
// them, and one that cannot be asked fails the whole answer — prune then
// keeps everything and says which daemon was silent. Refs: MGIT-239
func liveSandboxDigests(ctx context.Context, list func(context.Context) ([]daemonrec.Listed, error)) (map[string]bool, error) {
	listed, err := list(ctx)
	if err != nil {
		return nil, fmt.Errorf("list sandbox daemons: %w", err)
	}
	inUse := map[string]bool{}
	for _, d := range listed {
		if !d.Status.Alive {
			continue // a dead daemon runs nothing; its record is merely stale
		}
		askCtx, cancel := context.WithTimeout(ctx, daemonAskTimeout)
		boxes, err := sandboxd.NewClient(d.Record.Socket, time.Now).List(askCtx)
		cancel()
		if err != nil {
			return nil, daemonSilence(d.Record, err)
		}
		for _, sb := range boxes {
			if occupiesBase(sb.State) {
				inUse[sb.ImageDigest] = true
			}
		}
	}
	return inUse, nil
}

// occupiesBase reports whether a sandbox in this state boots, or will boot,
// from its base: a created sandbox boots lazily on first use.
func occupiesBase(state string) bool {
	switch state {
	case model.StateLanded, model.StateDestroyed, model.StateDead:
		return false
	}
	return true
}

// newSandboxBasePruneCmd removes the guest-base cache entries nothing pins
// any more. Refs: MGIT-239
func newSandboxBasePruneCmd(open func() (baseprune.Deps, error)) *cobra.Command {
	var dryRun bool
	var unknown []string
	cmd := &cobra.Command{
		Use:   "prune",
		Short: "Remove guest-base cache entries that no repository pins any more",
		Long: "Every repository that pins a cached guest base is recorded beside the cache.\n" +
			"prune reads each recorded repository's CURRENT images.lock and removes an\n" +
			"entry only when every one of them has stopped pinning it or no longer\n" +
			"exists, and no sandbox runs on it. Each live sandbox daemon is asked; if\n" +
			"one cannot be asked, nothing is removed.\n\n" +
			"Entries composed before pinners were recorded say \"pinner unknown\" and\n" +
			"are kept; remove one only by naming it with --remove-unknown. Any later\n" +
			"compose, adopt or launch from a repository records it as a pinner.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			d, err := open()
			if err != nil {
				return err
			}
			return runBasePrune(cmd.Context(), cmd.OutOrStdout(), d, dryRun, unknown)
		},
	}
	cmd.Flags().BoolVar(&dryRun, "dry-run", false, "list every entry, its size, composer and pinners; remove nothing")
	cmd.Flags().StringArrayVar(&unknown, "remove-unknown", nil,
		"also remove this entry (sha256:<hex>) although nothing records its pinners; repeatable")
	return cmd
}

// runBasePrune plans, or plans and applies, and prints what it found.
func runBasePrune(ctx context.Context, out io.Writer, d baseprune.Deps, dryRun bool, unknown []string) error {
	if dryRun {
		if len(unknown) > 0 {
			return fmt.Errorf("base prune: --remove-unknown removes; drop --dry-run to use it")
		}
		items, err := baseprune.Plan(ctx, d)
		if err != nil {
			return err
		}
		renderPrunePlan(out, d.Cache.Root(), items)
		var bytes int64
		n := 0
		for _, it := range items {
			if it.Verdict == baseprune.Prunable {
				n++
				bytes += it.Bytes
			}
		}
		_, _ = fmt.Fprintf(out, "\n%d prunable, %s would be freed (dry run: nothing removed)\n", n, pruneBytes(bytes))
		return nil
	}
	res, err := baseprune.Apply(ctx, d, unknown)
	if err != nil && len(res.Items) == 0 {
		return err
	}
	renderPrunePlan(out, d.Cache.Root(), res.Items)
	for _, it := range res.Removed {
		_, _ = fmt.Fprintf(out, "removed %s (%s)\n", it.Digest, pruneBytes(it.Bytes))
	}
	_, _ = fmt.Fprintf(out, "\nremoved %d, freed %s\n", len(res.Removed), pruneBytes(res.Freed))
	return err
}

// renderPrunePlan prints one aligned row per entry, then, entry by entry, its
// recorded pinners and the reason it is kept. The details sit below the table
// rather than inside it: a reason is a sentence, and as a table cell it would
// widen every column.
func renderPrunePlan(out io.Writer, root string, items []baseprune.Item) {
	if len(items) == 0 {
		_, _ = fmt.Fprintf(out, "the guest-base cache at %s holds no entries\n", root)
		return
	}
	_, _ = fmt.Fprintf(out, "guest-base cache %s\n\n", root)
	tw := tabwriter.NewWriter(out, 0, 4, 2, ' ', 0)
	_, _ = fmt.Fprintln(tw, "DIGEST\tSIZE\tCOMPOSED BY\tVERDICT")
	for _, it := range items {
		_, _ = fmt.Fprintf(tw, "%s\t%s\t%s\t%s\n", it.Digest, pruneBytes(it.Bytes), it.ComposedBy, it.Verdict)
	}
	_ = tw.Flush() // a failed write to the terminal has nowhere better to be reported
	for _, it := range items {
		if len(it.Pinners) == 0 && it.Reason == "" {
			continue
		}
		_, _ = fmt.Fprintf(out, "\n%s\n", it.Digest)
		for _, p := range it.Pinners {
			_, _ = fmt.Fprintf(out, "  pinner %s: %s\n", p.Root, p.State)
		}
		if it.Reason != "" {
			_, _ = fmt.Fprintf(out, "  %s\n", it.Reason)
		}
	}
}

// pruneBytes renders a byte count in binary units, the way df reports them.
func pruneBytes(n int64) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	div, exp := int64(unit), 0
	for m := n / unit; m >= unit; m /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %siB", float64(n)/float64(div), strings.Split("K M G T P", " ")[exp])
}

// daemonSilence is the error for a daemon prune could not ask. Only the
// first line of the daemon's own error is kept: every entry's verdict repeats
// it, and a wire-version refusal runs to a page of upgrade steps that the
// table cannot carry. Refs: MGIT-239
func daemonSilence(rec daemonrec.Record, err error) error {
	first, _, _ := strings.Cut(err.Error(), "\n")
	return fmt.Errorf("the daemon for %s (pid %d) did not answer: %s", rec.RepoRoot, rec.PID, strings.TrimSpace(first))
}
