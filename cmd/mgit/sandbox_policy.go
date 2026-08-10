package main

import (
	"encoding/json"
	"fmt"
	"io"
	"strings"

	"github.com/spf13/cobra"

	"github.com/hyper-swe/mgit/internal/controlproto"
)

// sandboxPolicyCmd groups the LIVE egress-policy verbs: change what a RUNNING
// sandbox may reach, without relaunching it.
//
// WHY THIS EXISTS. Provisioning needs package-registry egress during setup and
// needs it gone before the untrusted dev/test run. Until now the only revoke
// was relaunch, which destroys the environment that was just provisioned — so
// callers held egress open for the whole run, a weaker posture than intended.
// Refs: MGIT-72, FR-17.8, FR-17.12, SEC-04
func sandboxPolicyCmd(connect connectFunc) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "policy",
		Short: "Change or show a RUNNING sandbox's egress policy (no relaunch)",
		Long: "Change or show a RUNNING sandbox's egress policy without relaunching it.\n\n" +
			"The authorizer is consulted per connection, so a mutation takes effect on the\n" +
			"next flow. Established flows are TERMINATED unless --drain is given.\n" +
			"Every mutation is recorded in the append-only sandbox audit trail.",
	}
	cmd.AddCommand(
		sandboxPolicySetCmd(connect),
		sandboxPolicyRevokeCmd(connect),
		sandboxPolicyShowCmd(connect),
	)
	return cmd
}

// establishedFlowNote is the established-flow decision, stated at every verb
// that can terminate a connection.
//
// It is repeated rather than referenced because kill and drain are OPPOSITE
// security postures: a caller who assumes the other one is exposed, and the
// place they will read is the help of the command they are about to run.
// Refs: MGIT-72, ADR-012
const establishedFlowNote = "\n\nESTABLISHED FLOWS ARE TERMINATED by default. A caller who revokes " +
	"registry\negress and then runs untrusted code expects the grant to be gone, and a\n" +
	"draining connection is precisely the exfiltration channel they just revoked —\n" +
	"a hostile guest can hold one open arbitrarily long, so \"drain\" can mean\n" +
	"\"never\". Pass --drain to leave established flows to finish instead."

// sandboxPolicySetCmd replaces the allowlist with an explicit set of
// destinations.
func sandboxPolicySetCmd(connect connectFunc) *cobra.Command {
	var taskID string
	var allow []string
	var drain, asJSON bool
	cmd := &cobra.Command{
		Use:   "set --task <id> --allow <host:port> [--allow ...]",
		Short: "Replace a running sandbox's egress allowlist",
		Long: "Replace a running sandbox's egress allowlist in ONE atomic change.\n\n" +
			"The replacement is compiled before it is swapped in, so a flow is authorized\n" +
			"against the old policy or the new one and never a mixture; a policy that does\n" +
			"not compile leaves the running one in force." + establishedFlowNote,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if taskID == "" {
				return fmt.Errorf("--task-id is required")
			}
			// An empty `set` and a `revoke` are the same operation underneath.
			// Refusing it here is deliberate: a caller who meant to grant and
			// mistyped the flag would otherwise silently revoke everything.
			if len(allow) == 0 {
				return fmt.Errorf(
					"--allow is required: `policy set` with no destinations would revoke ALL " +
						"egress — say so with `mgit sandbox policy revoke` if that is the intent")
			}
			cl, err := connect(cmd.Context())
			if err != nil {
				return err
			}
			res, err := cl.SetEgressPolicy(cmd.Context(), taskID, allow, drain)
			if err != nil {
				return err
			}
			return printPolicy(cmd.OutOrStdout(), res, asJSON)
		},
	}
	bindTaskIDFlag(cmd, &taskID, "task whose running sandbox to change (required)")
	cmd.Flags().StringArrayVar(&allow, "allow", nil,
		"destination to permit (host:port, ip, or CIDR); repeat for several")
	bindPolicyOutputFlags(cmd, &drain, &asJSON)
	return cmd
}

// sandboxPolicyRevokeCmd removes ALL egress from a running sandbox.
func sandboxPolicyRevokeCmd(connect connectFunc) *cobra.Command {
	var taskID string
	var drain, asJSON bool
	cmd := &cobra.Command{
		Use:   "revoke --task <id>",
		Short: "Revoke ALL egress from a running sandbox",
		Long: "Revoke ALL egress from a running sandbox, without relaunching it.\n\n" +
			"This is the sequence provisioning runs: grant registry egress for setup, then\n" +
			"revoke it before the untrusted dev/test run." + establishedFlowNote,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if taskID == "" {
				return fmt.Errorf("--task-id is required")
			}
			cl, err := connect(cmd.Context())
			if err != nil {
				return err
			}
			// nil entries: the replacement policy permits nothing.
			res, err := cl.SetEgressPolicy(cmd.Context(), taskID, nil, drain)
			if err != nil {
				return err
			}
			return printPolicy(cmd.OutOrStdout(), res, asJSON)
		},
	}
	bindTaskIDFlag(cmd, &taskID, "task whose running sandbox to revoke (required)")
	bindPolicyOutputFlags(cmd, &drain, &asJSON)
	return cmd
}

// sandboxPolicyShowCmd reports the policy IN FORCE.
func sandboxPolicyShowCmd(connect connectFunc) *cobra.Command {
	var taskID string
	var asJSON bool
	cmd := &cobra.Command{
		Use:   "show --task <id>",
		Short: "Show the egress policy a running sandbox is enforcing right now",
		Long: "Show the egress policy a running sandbox is enforcing RIGHT NOW.\n\n" +
			"This is not the launch-time policy on `mgit sandbox status`: once a live\n" +
			"mutation has happened the two disagree, and the launch one would say egress\n" +
			"is open when it has been revoked.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if taskID == "" {
				return fmt.Errorf("--task-id is required")
			}
			cl, err := connect(cmd.Context())
			if err != nil {
				return err
			}
			res, err := cl.EgressPolicy(cmd.Context(), taskID)
			if err != nil {
				return err
			}
			return printPolicy(cmd.OutOrStdout(), res, asJSON)
		},
	}
	bindTaskIDFlag(cmd, &taskID, "task whose live policy to show (required)")
	cmd.Flags().BoolVar(&asJSON, "json", false, "output as JSON")
	return cmd
}

// bindPolicyOutputFlags binds the flags shared by the mutating verbs.
func bindPolicyOutputFlags(cmd *cobra.Command, drain, asJSON *bool) {
	cmd.Flags().BoolVar(drain, "drain", false,
		"leave ESTABLISHED connections to finish instead of terminating them "+
			"(weaker: a draining connection is the channel you just revoked)")
	cmd.Flags().BoolVar(asJSON, "json", false, "output as JSON")
}

// printPolicy renders the outcome. It states what is IN FORCE and what the
// change did to established flows — outcomes, not intentions, so a caller can
// tell a revoke that terminated two connections from one that found none.
func printPolicy(w io.Writer, res *controlproto.PolicyResult, asJSON bool) error {
	if res == nil {
		return fmt.Errorf("the daemon returned no policy result")
	}
	if asJSON {
		return json.NewEncoder(w).Encode(res)
	}
	if len(res.Entries) == 0 {
		_, _ = fmt.Fprintln(w, "egress policy in force: no egress permitted (0 rules)")
	} else {
		_, _ = fmt.Fprintf(w, "egress policy in force: %s (%d rules)\n",
			strings.Join(res.Entries, ", "), res.RuleCount)
	}
	switch {
	case res.Drained:
		_, _ = fmt.Fprintln(w,
			"established flows: DRAINED — existing connections were left to finish")
	default:
		_, _ = fmt.Fprintf(w,
			"established flows: %d terminated\n", res.Killed)
	}
	return nil
}
