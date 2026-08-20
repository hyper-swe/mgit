package agentadapter

import (
	"fmt"
	"strings"
)

// Routing is how strongly mgit can make ONE agent family's shell commands
// reach the task sandbox. It is a second, independent axis from Containment:
// Containment answers "is there a guest?", Routing answers "can the agent's
// command avoid it?". Reporting only the first is what made an advisory lane
// indistinguishable from an enforced one — the agent believed it was
// contained, the operator believed it, and untrusted code ran on the host.
// Refs: MGIT-149, MGIT-47
type Routing int

const (
	// RoutingRouted means the harness invokes an mgit hook for every shell
	// call and the hook REWRITES the command to run inside the guest. The
	// agent does not choose to be routed and cannot decline. Refs: MGIT-149
	RoutingRouted Routing = iota
	// RoutingBlocked means the harness invokes an mgit hook for every shell
	// call, but the hook may only permit or refuse — it cannot rewrite. An
	// uncontained command is REFUSED with instructions, so it never runs on
	// the host silently; the agent must re-issue it through `mgit run`.
	// Weaker than Routed in ergonomics, identical in the guarantee that
	// matters: nothing reaches the host unannounced. Refs: MGIT-149
	RoutingBlocked
	// RoutingAdvisory means nothing intercepts the command. A PATH shim and
	// written instructions are all that route it, and any process can reset
	// PATH or call a binary by absolute path. This is ADVICE, and must never
	// be described in words that imply a guarantee. Refs: MGIT-149
	RoutingAdvisory
)

// String renders the tier as the single word used in machine-parseable
// output and in prose. Refs: MGIT-149
func (r Routing) String() string {
	switch r {
	case RoutingRouted:
		return "routed"
	case RoutingBlocked:
		return "blocked"
	default:
		return "advisory"
	}
}

// Family is one agent harness and the mechanism, if any, by which mgit can
// make its shell commands reach the sandbox. Config names the file that
// carries the mechanism so an operator can verify the claim by opening it
// rather than trusting this table. Refs: MGIT-149
type Family struct {
	ID        string  // stable lowercase identifier used by --agent
	Display   string  // human name
	Routing   Routing // what mgit can actually enforce for this family
	Config    string  // worktree-relative file carrying the mechanism
	Mechanism string  // one clause describing how it works
}

// Families is the enforcement matrix, ordered strongest first.
//
// The tiers are not mgit's choice — they are what each harness's hook API
// permits, verified against vendor documentation:
//
//	Claude Code  PreToolUse returns updatedInput, so the command is rewritten.
//	Codex        PreToolUse returns updatedInput likewise. Its "ask" decision
//	             is parsed but NOT honored, so mgit's fail-closed fallback for
//	             Codex must be deny, not ask (see DecideCodex).
//	Cursor       beforeShellExecution returns a permission only — there is no
//	             field that can replace the command — so the strongest posture
//	             available is refusal.
//	generic      No harness contract at all. PATH shims and prose only.
//
// Refs: MGIT-149, FEAT-3.71
func Families() []Family {
	return []Family{
		{
			ID: "claude", Display: "Claude Code", Routing: RoutingRouted,
			Config:    ClaudeSettingsFile,
			Mechanism: "PreToolUse hook rewrites every Bash call to run inside the guest",
		},
		{
			ID: "codex", Display: "Codex", Routing: RoutingRouted,
			Config:    CodexHooksFile,
			Mechanism: "PreToolUse hook rewrites every Bash call to run inside the guest",
		},
		{
			ID: "cursor", Display: "Cursor", Routing: RoutingBlocked,
			Config:    CursorHooksFile,
			Mechanism: "beforeShellExecution hook refuses any command that is not routed (it cannot rewrite)",
		},
		{
			ID: "generic", Display: "Other/unknown harness", Routing: RoutingAdvisory,
			Config:    EnvrcFile,
			Mechanism: "PATH shims plus written instructions — advisory, since any process can reset PATH",
		},
	}
}

// LookupFamily resolves a family by its ID, case-insensitively. Refs: MGIT-149
func LookupFamily(id string) (Family, bool) {
	want := strings.ToLower(strings.TrimSpace(id))
	for _, f := range Families() {
		if f.ID == want {
			return f, true
		}
	}
	return Family{}, false
}

// FamilyIDs lists the valid --agent values, for flag help and error text.
// Refs: MGIT-149
func FamilyIDs() []string {
	ids := make([]string, 0, len(Families()))
	for _, f := range Families() {
		ids = append(ids, f.ID)
	}
	return ids
}

// RoutingReport is the human block `mgit work` prints when it provisions a
// contained worktree. It exists because the previous output said only
// "Containment: active", which is true of the GUEST and says nothing about
// whether a given agent can step around it. Every family is named, with its
// tier and the file to check. Refs: MGIT-149
func RoutingReport() string {
	var b strings.Builder
	b.WriteString("Command routing, per agent family (containment is only as good as the routing):\n")
	for _, f := range Families() {
		fmt.Fprintf(&b, "  %-22s %-9s %s — %s\n", f.Display, f.Routing, f.Config, f.Mechanism)
	}
	b.WriteString("  Routed and blocked lanes cannot silently reach the host. " +
		"The advisory lane can: verify with `hostname; whoami` inside the agent.\n")
	return b.String()
}

// RoutingStatusLines is the machine-parseable form of the report: one
// "Routing: <id>=<tier>" line per family, so a harness or script reads the
// posture without parsing prose. Refs: MGIT-149, MGIT-47
func RoutingStatusLines() []string {
	lines := make([]string, 0, len(Families()))
	for _, f := range Families() {
		lines = append(lines, fmt.Sprintf("Routing: %s=%s", f.ID, f.Routing))
	}
	return lines
}
