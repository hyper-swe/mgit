package main

import (
	"encoding/json"
	"fmt"
	"io"
	"os"

	"github.com/spf13/cobra"

	"github.com/hyper-swe/mgit/internal/reviewbind"
	"github.com/hyper-swe/mgit/internal/sandboxd/images"
)

// sandboxReviewMapCmd is the operator's way to install and read the binding
// maps a review run is checked against. It touches only the host root's files;
// it never talks to the daemon and never prints a credential.
func sandboxReviewMapCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "map",
		Short: "Install and show the operator's review binding maps",
	}
	cmd.AddCommand(sandboxReviewMapInstallCmd(), sandboxReviewMapShowCmd())
	return cmd
}

// mapImageNames asks the host root's image store (images.lock) which names are
// registered under a digest. A variable so a test can stand in for the store.
var mapImageNames reviewbind.ImageNames = images.NamesForDigest

func mapHostRoot(override string) (string, error) {
	if override != "" {
		return override, nil
	}
	return sandboxHostRoot()
}

func printInfo(w io.Writer, info reviewbind.Info, asJSON bool) error {
	if asJSON {
		raw, err := json.Marshal(info)
		if err != nil {
			return err
		}
		_, err = fmt.Fprintln(w, string(raw))
		return err
	}
	_, err := fmt.Fprintf(w, "id: %s\nbinding_sha256: %s\nimage_ref: %s\nimage_digest: %s\nmap_sha256: %s\negress_policy_id: %s\n", info.ID, info.BindingSHA256, info.ImageRef, info.ImageDigest, info.MapSHA256, info.EgressPolicyID)
	return err
}

func sandboxReviewMapInstallCmd() *cobra.Command {
	var file, root string
	var makeDefault, asJSON bool
	cmd := &cobra.Command{
		Use:   "install --file <map.json> [--default]",
		Short: "Install a review binding map and pin its hash",
		Long: "Checks a mgit.review-map/v1 record, writes it with its pinned hash under the host root's review/maps\n" +
			"directory (owner-only, never overwriting an installed id) and verifies it loads. --default also makes it\n" +
			"the map `show` reports when no id is given. An egress policy `host:<fqdn>` opens that host on port 443\n" +
			"only, resolved when the review starts; the grant does not follow later DNS changes. The host root is the one the\n" +
			"sandbox review commands use; --host-root overrides it.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			hostRoot, err := mapHostRoot(root)
			if err != nil {
				return err
			}
			raw, err := os.ReadFile(file) //nolint:gosec // the operator names their own map file
			if err != nil {
				return fmt.Errorf("read the map file: %w", err)
			}
			info, err := reviewbind.Install(hostRoot, raw, makeDefault)
			if err != nil {
				cmd.SilenceUsage = true
				return err
			}
			return printInfo(cmd.OutOrStdout(), info, asJSON)
		},
	}
	f := cmd.Flags()
	f.StringVar(&file, "file", "", "the map record to install")
	f.StringVar(&root, "host-root", "", "host root (default: the one the sandbox review commands use)")
	f.BoolVar(&makeDefault, "default", false, "make this the default map")
	f.BoolVar(&asJSON, "json", false, "print the result as mgit.review-map-info/v1 JSON")
	_ = cmd.MarkFlagRequired("file")
	return cmd
}

func sandboxReviewMapShowCmd() *cobra.Command {
	var id, root string
	var asJSON bool
	cmd := &cobra.Command{
		Use:   "show [--id <id>]",
		Short: "Verify an installed review binding map and describe it",
		Long: "Verifies the map against its pinned hash and prints its id, the binding digest the record carries and\n" +
			"the map hash that was pinned. Without --id it describes the default map, or says how to install one.\n" +
			"It never prints a credential.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			hostRoot, err := mapHostRoot(root)
			if err != nil {
				return err
			}
			info, err := reviewbind.Show(hostRoot, id, mapImageNames)
			if err != nil {
				cmd.SilenceUsage = true
				return err
			}
			return printInfo(cmd.OutOrStdout(), info, asJSON)
		},
	}
	f := cmd.Flags()
	f.StringVar(&id, "id", "", "map id (default: the default map)")
	f.StringVar(&root, "host-root", "", "host root (default: the one the sandbox review commands use)")
	f.BoolVar(&asJSON, "json", false, "print the result as mgit.review-map-info/v1 JSON")
	return cmd
}
