package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"

	"github.com/spf13/cobra"

	"github.com/hyper-swe/mgit/internal/controlproto"
	"github.com/hyper-swe/mgit/internal/model"
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
			"not compile leaves the running one in force.\n\n" +
			"A sandbox whose microVM has not booted yet (lazy provisioning) is not an error:\n" +
			"the policy is STAGED onto its pending launch and the VM comes up enforcing it,\n" +
			"reported as PENDING so it is never mistaken for one in force." +
			establishedFlowNote + policyFailureCodes,
		Args: cobra.NoArgs,
		// The failure is rendered exactly once, by runPolicy, in the shape
		// the caller asked for: cobra must not also print it as human text
		// over a --json response, nor dump usage over a runtime failure.
		// Refs: MGIT-109, R-H233
		SilenceErrors: true,
		SilenceUsage:  true,
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
			return runPolicy(cmd, asJSON, func() (*controlproto.PolicyResult, error) {
				return cl.SetEgressPolicy(cmd.Context(), taskID, allow, drain)
			})
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
			"revoke it before the untrusted dev/test run." +
			establishedFlowNote + policyFailureCodes,
		Args: cobra.NoArgs,
		// The failure is rendered exactly once, by runPolicy, in the shape
		// the caller asked for: cobra must not also print it as human text
		// over a --json response, nor dump usage over a runtime failure.
		// Refs: MGIT-109, R-H233
		SilenceErrors: true,
		SilenceUsage:  true,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if taskID == "" {
				return fmt.Errorf("--task-id is required")
			}
			cl, err := connect(cmd.Context())
			if err != nil {
				return err
			}
			return runPolicy(cmd, asJSON, func() (*controlproto.PolicyResult, error) {
				// nil entries: the replacement policy permits nothing.
				return cl.SetEgressPolicy(cmd.Context(), taskID, nil, drain)
			})
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
			"is open when it has been revoked.\n\n" +
			"For a sandbox whose microVM has not booted yet, this reports the policy it\n" +
			"WILL enforce, labeled PENDING. A pending policy is never presented as one in\n" +
			"force: \"is being enforced\" and \"will be enforced once something starts\" are\n" +
			"different facts." + policyFailureCodes,
		Args: cobra.NoArgs,
		// The failure is rendered exactly once, by runPolicy, in the shape
		// the caller asked for: cobra must not also print it as human text
		// over a --json response, nor dump usage over a runtime failure.
		// Refs: MGIT-109, R-H233
		SilenceErrors: true,
		SilenceUsage:  true,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if taskID == "" {
				return fmt.Errorf("--task-id is required")
			}
			cl, err := connect(cmd.Context())
			if err != nil {
				return err
			}
			return runPolicy(cmd, asJSON, func() (*controlproto.PolicyResult, error) {
				return cl.EgressPolicy(cmd.Context(), taskID)
			})
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
	// A PENDING policy is never described as one in force. The sandbox is
	// registered and its microVM has not booted (lazy provisioning), so these
	// entries are what it WILL enforce — and a caller who read that as "in
	// force" would run untrusted code believing a line is being held that
	// nothing is holding yet. Refs: MGIT-109, FR-17.10, SEC-04
	if res.Pending {
		return printPendingPolicy(w, res)
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

// printPendingPolicy renders a policy staged onto a sandbox that has not
// booted. It says PENDING first and never prints an established-flow count: a
// VM that has never run has no flows, and "0 terminated" would read as a
// reassurance about a revoke that has not happened yet. Refs: MGIT-109
func printPendingPolicy(w io.Writer, res *controlproto.PolicyResult) error {
	if len(res.Entries) == 0 {
		_, _ = fmt.Fprintln(w, "egress policy PENDING: no egress will be permitted (0 entries)")
	} else {
		_, _ = fmt.Fprintf(w, "egress policy PENDING: %s\n", strings.Join(res.Entries, ", "))
	}
	_, _ = fmt.Fprintln(w,
		"not yet enforced — this sandbox's microVM has not booted; it will come up "+
			"enforcing this policy on first use (`mgit run -- <cmd>`)")
	return nil
}

// policyFailureCodes documents the stable tokens on every policy verb's help.
//
// It is repeated on all three verbs rather than referenced, for the same reason
// the established-flow note is: the place an integrator reads is the help of the
// command they are about to script. Refs: MGIT-109, R-H233
const policyFailureCodes = "\n\nFAILURE CODES (stable contract). Every failure of this verb carries a " +
	"machine-readable\ntoken — as `error_code` in --json output, and in square brackets at the start of\n" +
	"the human message. Match on the TOKEN, never on the wording, which will change:\n\n" +
	"  NOT_BOOTED        the sandbox is registered but its microVM has not booted;\n" +
	"                    nothing is enforcing egress for it yet.\n" +
	"  BOOTED_DIED       it is recorded as running but its enforcer is not answering:\n" +
	"                    the guest exited or was killed. Tear it down and relaunch.\n" +
	"  VERSION_PREDATES  its VM was launched without a control channel by an older\n" +
	"                    build; its launch-time allowlist stands. Relaunch it.\n" +
	"  UNKNOWN           anything this build cannot classify. It is never collapsed\n" +
	"                    into one of the above."

// runPolicy runs one policy verb, rendering a failure as structured JSON when
// --json was asked for.
//
// The failure path is the one that MATTERS here: a consumer's pre-boot retry
// matched on error wording and silently missed this very failure, so an exit
// code alone is not enough — the token has to be readable from the error
// itself, in both output modes. Refs: MGIT-109, R-H233
func runPolicy(
	cmd *cobra.Command, asJSON bool, op func() (*controlproto.PolicyResult, error),
) error {
	res, err := op()
	if err == nil {
		return printPolicy(cmd.OutOrStdout(), res, asJSON)
	}
	// The verbs run under SilenceErrors, so the failure is rendered exactly
	// once and in the shape the caller asked for. --json gets an object with
	// the stable token; the human path gets the message, whose text already
	// begins with that token. The error is still returned, so the process
	// exits non-zero either way.
	if asJSON {
		if encErr := json.NewEncoder(cmd.OutOrStdout()).Encode(policyErrorJSON{
			Error: err.Error(), ErrorCode: policyFailureCode(err),
		}); encErr != nil {
			return encErr
		}
		return err
	}
	_, _ = fmt.Fprintf(cmd.ErrOrStderr(), "mgit sandbox: %v\n", err)
	return err
}

// policyErrorJSON is the --json failure shape. error_code is the contract;
// error is the prose, which is free to change. Refs: MGIT-109, R-H233
type policyErrorJSON struct {
	Error     string `json:"error"`
	ErrorCode string `json:"error_code"`
}

// policyFailureCode resolves a failure to its stable token, defaulting to
// UNKNOWN. It never guesses at one of the specific three: an unclassifiable
// failure reported as NOT_BOOTED would be a confident wrong answer, which is
// the defect this ticket is about one layer down. Refs: MGIT-109, R-H233
func policyFailureCode(opErr error) string {
	var failure *model.EgressPolicyError
	if errors.As(opErr, &failure) && model.ValidEgressFailureCode(failure.Code) {
		return failure.Code
	}
	return model.EgressFailureUnknown
}
