package main

import (
	"fmt"
	"io"
	"strings"

	"github.com/hyper-swe/mgit/internal/agentadapter"
)

// resolveRoutingFamily validates the --agent / --require-routing pair and
// returns the declared family, or nil when none was declared.
//
// THE ARGUED CHOICE (MGIT-149 asks for one rather than an assumption).
// `mgit work --sandbox` does NOT refuse to provision when routing for a family
// is merely advisory, and the reason is that the advisory family IS the
// unknown-harness family. Refusing by default would block every harness mgit
// has not written an adapter for — a refusal that blocks a working lane is its
// own defect, and it would push operators to drop --sandbox altogether, which
// removes the guest as well as the advice. So the default is: provision, and
// state the tier loudly enough that an operator can act on it.
//
// What an operator who needs a guarantee gets instead is `--require-routing`,
// which turns that verdict into a refusal. It requires --agent, because
// without a declared family there is no single tier to test: the worktree is
// wired for all of them at once.
//
// Refs: MGIT-149
func resolveRoutingFamily(opts workOptions) (*agentadapter.Family, error) {
	if opts.Agent == "" {
		if opts.RequireRouting {
			return nil, fmt.Errorf("--require-routing needs --agent <%s>: without a declared family "+
				"there is no single routing tier to require (the worktree is wired for all of them)",
				strings.Join(agentadapter.FamilyIDs(), "|"))
		}
		return nil, nil
	}
	fam, ok := agentadapter.LookupFamily(opts.Agent)
	if !ok {
		return nil, fmt.Errorf("unknown --agent %q: valid families are %s",
			opts.Agent, strings.Join(agentadapter.FamilyIDs(), ", "))
	}
	if opts.RequireRouting && fam.Routing == agentadapter.RoutingAdvisory {
		return nil, fmt.Errorf("--require-routing: routing for %s is advisory — its commands can reach the "+
			"host without passing through the sandbox, and mgit has no mechanism to stop them. "+
			"Use an agent family with a harness hook (%s), or drop --require-routing to provision anyway",
			fam.Display, strings.Join(routedFamilyIDs(), ", "))
	}
	return &fam, nil
}

// routedFamilyIDs lists the families whose commands cannot reach the host
// unannounced, for use in remediation text. Refs: MGIT-149
func routedFamilyIDs() []string {
	ids := make([]string, 0, len(agentadapter.Families()))
	for _, f := range agentadapter.Families() {
		if f.Routing != agentadapter.RoutingAdvisory {
			ids = append(ids, f.ID)
		}
	}
	return ids
}

// reportRouting prints the routing posture at provisioning time: the full
// per-family matrix, plus a single unmissable verdict when --agent named one.
// Refs: MGIT-149
func reportRouting(out io.Writer, fam *agentadapter.Family, declared string) {
	_, _ = fmt.Fprint(out, agentadapter.RoutingReport())
	for _, line := range agentadapter.RoutingStatusLines() {
		_, _ = fmt.Fprintln(out, line)
	}
	if fam == nil {
		return
	}
	_, _ = fmt.Fprintln(out, routingVerdict(*fam, declared))
}

// routingVerdict is the one line an operator who declared a family must read.
// The advisory case is deliberately shouted: it is the only tier where a
// command can reach the host with nothing announcing it. Refs: MGIT-149
func routingVerdict(fam agentadapter.Family, declared string) string {
	switch fam.Routing {
	case agentadapter.RoutingRouted:
		return fmt.Sprintf("Declared agent %s (%s): ENFORCED — %s routes every shell command into the guest.",
			declared, fam.Display, fam.Config)
	case agentadapter.RoutingBlocked:
		return fmt.Sprintf("Declared agent %s (%s): ENFORCED by refusal — %s cannot rewrite a command, so an "+
			"uncontained one is blocked with instructions rather than run on the host.",
			declared, fam.Display, fam.Config)
	default:
		return fmt.Sprintf("Declared agent %s (%s): ADVISORY — nothing intercepts its commands. PATH shims and "+
			"written instructions are all that route them; a reset PATH or an absolute path reaches the host, "+
			"uncontained, with no warning. Verify inside the agent with `hostname; whoami`.",
			declared, fam.Display)
	}
}
