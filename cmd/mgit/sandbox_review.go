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

// reviewPreparer is the one verb `review prepare` needs. It is asserted on the
// connected client rather than added to sandboxClient, so a client without it
// reports that it cannot ask. Refs: MGIT-333
type reviewPreparer interface {
	ReviewPrepare(ctx context.Context, args controlproto.ReviewPrepareArgs) ([]byte, error)
}

func sandboxReviewCmd(connect connectFunc) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "review",
		Short: "Prepare and run an isolated, read-only review",
		Long:  "prepare checks a review request and signs exactly what would run; it starts nothing. run runs a prepared review.",
	}
	cmd.AddCommand(sandboxReviewPrepareCmd(connect))
	cmd.AddCommand(sandboxReviewRunCmd(connect))
	cmd.AddCommand(sandboxReviewMapCmd())
	return cmd
}

func sandboxReviewPrepareCmd(connect connectFunc) *cobra.Command {
	var a controlproto.ReviewPrepareArgs
	var format string
	cmd := &cobra.Command{
		Use:   "prepare",
		Short: "Check a review request and print a signed attestation of what would run",
		Long: "Checks the request against the operator-installed binding map and the detached evidence\n" +
			"directory (exactly one file, evidence.json), seals the evidence under a short-lived lease, and\n" +
			"prints the signed mgit.review-attest/v1 as JSON on standard output. It never starts the reviewer.\n\n" +
			"A refusal prints an mgit.review-error/v1 envelope on standard error, with reviewer_started=false,\n" +
			"prints nothing on standard output, and exits nonzero.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if format != "json" {
				return fmt.Errorf("--format must be json")
			}
			cl, err := connect(cmd.Context())
			if err != nil {
				return err
			}
			rp, ok := cl.(reviewPreparer)
			if !ok {
				return errors.New("this client cannot ask the daemon to prepare a review")
			}
			a.BundleDir = canonicalPath(a.BundleDir)
			att, err := rp.ReviewPrepare(cmd.Context(), a)
			var refusal *reviewprep.Error
			if errors.As(err, &refusal) {
				raw, _ := json.Marshal(refusal)
				fmt.Fprintln(cmd.ErrOrStderr(), string(raw))
				cmd.SilenceUsage = true
				return fmt.Errorf("review refused: %s", refusal.Code)
			}
			if err != nil {
				return err
			}
			_, err = fmt.Fprintln(cmd.OutOrStdout(), string(att))
			return err
		},
	}
	f := cmd.Flags()
	f.StringVar(&a.Profile, "profile", "", "review profile (reviewer-ro/v1)")
	f.StringVar(&a.Image, "image", "", "guest image as <name>@sha256:<digest>")
	f.StringVar(&a.MappingID, "mapping", "", "operator-installed binding map id")
	f.StringVar(&a.BindingDigest, "binding-digest", "", "sha256 of the host binding")
	f.StringVar(&a.BundleDir, "bundle", "", "detached directory holding evidence.json")
	f.StringVar(&a.BundleDigest, "bundle-digest", "", "sha256 of the exact evidence.json bytes")
	f.StringVar(&a.Nonce, "nonce", "", "caller-chosen random nonce")
	f.StringVar(&format, "format", "", "output format (json)")
	for _, n := range []string{"profile", "image", "mapping", "binding-digest", "bundle", "bundle-digest", "nonce", "format"} {
		_ = cmd.MarkFlagRequired(n)
	}
	return cmd
}
