package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/spf13/cobra"

	"github.com/hyper-swe/mgit/internal/controlproto"
	"github.com/hyper-swe/mgit/internal/reviewprep"
)

// Exit statuses of a review run that did not run to its end. A reviewer's own
// exit status passes through unchanged except these two, which would otherwise
// read as a refusal.
const (
	reviewExitRefused = 70 // refused before the reviewer started
	reviewExitStopped = 71 // the reviewer started or may have started, and the run did not complete
)

// reviewRunner is the one verb `review run` needs, asserted on the connected
// client like reviewPreparer. Refs: MGIT-333
type reviewRunner interface {
	ReviewRun(ctx context.Context, args controlproto.ReviewRunArgs) ([]byte, error)
}

func sandboxReviewRunCmd(connect connectFunc) *cobra.Command {
	var a controlproto.ReviewRunArgs
	var format string
	cmd := &cobra.Command{
		Use:   "run",
		Short: "Run a prepared review and return what the reviewer produced",
		Long: "Runs the review prepared under --lease: the daemon re-verifies the sealed lease and the mapping,\n" +
			"consumes the nonce (a second run of the same lease is refused), starts the review guest, runs the\n" +
			"operator-mapped program and removes the guest. What runs comes from the mapping; there is no flag to\n" +
			"name a program, an argument or an environment.\n\n" +
			"By default standard output and standard error are the reviewer's own and the exit status is the\n" +
			"reviewer's (70 and 71 from a reviewer are reported as 1). With --format json one mgit.review-result/v1\n" +
			"object is printed and the exit status is 0 whenever the reviewer ran to its end.\n\n" +
			"A refusal prints one mgit.review-error/v1 envelope on standard error, prints nothing on standard\n" +
			"output, and exits 70 when the reviewer certainly did not start, or 71 when the reviewer started or may\n" +
			"have started (the run did not complete; treat the reviewer as having run).",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			cmd.SilenceUsage = true // an argument problem was caught above; what follows is not a usage error
			if format != "" && format != "json" {
				return fmt.Errorf("--format must be json or empty")
			}
			cl, err := connect(cmd.Context())
			if err != nil {
				return err
			}
			rr, ok := cl.(reviewRunner)
			if !ok {
				return errors.New("this client cannot ask the daemon to run a review")
			}
			raw, err := rr.ReviewRun(cmd.Context(), a)
			var refusal *reviewprep.Error
			if errors.As(err, &refusal) {
				env, _ := json.Marshal(refusal)
				fmt.Fprintln(cmd.ErrOrStderr(), string(env))
				cmd.SilenceErrors = true // the envelope is the whole of standard error
				if refusal.ReviewerStarted {
					return &exitError{code: reviewExitStopped}
				}
				return &exitError{code: reviewExitRefused}
			}
			if err != nil {
				return err
			}
			if format == "json" {
				_, err = fmt.Fprintln(cmd.OutOrStdout(), string(raw))
				return err
			}
			var res controlproto.ReviewResult
			if err := json.Unmarshal(raw, &res); err != nil || res.Schema != controlproto.ReviewResultSchema {
				return errors.New("the daemon's answer to the review run is not a review result")
			}
			if _, err := cmd.OutOrStdout().Write(res.Stdout); err != nil {
				return err
			}
			if _, err := cmd.ErrOrStderr().Write(res.Stderr); err != nil {
				return err
			}
			cmd.SilenceErrors = true // standard error is the reviewer's, with nothing added
			code := res.ExitCode
			if code == reviewExitRefused || code == reviewExitStopped {
				code = 1
			}
			if code != 0 {
				return &exitError{code: code}
			}
			return nil
		},
	}
	f := cmd.Flags()
	f.StringVar(&a.Profile, "profile", "", "review profile (reviewer-ro/v1)")
	f.StringVar(&a.Image, "image", "", "guest image as <name>@sha256:<digest>, as prepared")
	f.StringVar(&a.MappingID, "mapping", "", "operator-installed binding map id, as prepared")
	f.StringVar(&a.BindingDigest, "binding-digest", "", "sha256 of the host binding, as prepared")
	f.StringVar(&a.BundleDigest, "bundle-digest", "", "sha256 of the evidence.json bytes, as prepared")
	f.StringVar(&a.Nonce, "nonce", "", "the nonce the review was prepared with; usable once")
	f.StringVar(&a.Lease, "lease", "", "the lease id the prepare step sealed")
	f.IntVar(&a.MaxSeconds, "max-seconds", 0, fmt.Sprintf("wall-clock limit (default %d, at most %d)", controlproto.DefaultReviewSeconds, controlproto.MaxReviewSeconds))
	f.Int64Var(&a.MaxOutputBytes, "max-output-bytes", 0, fmt.Sprintf("limit on standard output and error together (default %d, at most %d)", controlproto.DefaultReviewOutputBytes, controlproto.MaxReviewOutputBytes))
	f.StringVar(&format, "format", "", "output format (json)")
	for _, n := range []string{"profile", "image", "mapping", "binding-digest", "bundle-digest", "nonce", "lease"} {
		_ = cmd.MarkFlagRequired(n)
	}
	return cmd
}
