package model

import "fmt"

// Live egress-policy mutation types (MGIT-72).
//
// A sandbox's LAUNCH policy is on SandboxInfo. These types describe the policy
// a RUNNING sandbox is enforcing right now, which stops being the launch
// policy the moment it is mutated — and reporting the launch policy after a
// revoke would tell a caller egress is open when it is closed.

// EgressPolicyState is the allowlist a running sandbox is enforcing.
// Refs: MGIT-72, FR-17.8
type EgressPolicyState struct {
	// Entries is the allowlist in force. Empty means nothing is permitted —
	// which is a real, enforced state, and is why an UNKNOWN sandbox must be
	// an error rather than an empty state.
	Entries []string `json:"entries"`
	// RuleCount is what those entries compiled to, so a caller can tell a
	// policy that compiled to nothing from one that was never applied.
	// It is 0 for a PENDING policy: nothing has compiled it yet.
	RuleCount int `json:"rule_count"`
	// Pending says these entries are NOT being enforced yet — the sandbox is
	// registered but its VM has not booted (lazy provisioning, FR-17.10), so
	// this is the policy it WILL boot with, not one in force.
	//
	// It exists because reporting a staged policy as an enforced one is the
	// same lie as reporting an empty policy for an unreachable enforcer: the
	// caller runs untrusted code believing something is holding a line that
	// nothing is holding yet. Refs: MGIT-109, MGIT-72, FR-17.10, SEC-04
	Pending bool `json:"pending,omitempty"`
}

// EgressPolicyChange reports what a live policy mutation actually did —
// outcomes, not intentions. Refs: MGIT-72, ADR-012
type EgressPolicyChange struct {
	// Entries is the policy now in force (read back from the enforcer, not
	// echoed from the request).
	Entries []string `json:"entries"`
	// RuleCount is the rule count the new policy compiled to.
	RuleCount int `json:"rule_count"`
	// Killed counts ESTABLISHED flows terminated by the change. It is the
	// number that carries "revoke means revoke": a revoke that swaps the
	// ruleset but leaves the open socket running is the failure this whole
	// capability exists to prevent.
	Killed int `json:"killed"`
	// Drained reports that established flows were deliberately left to
	// finish (the opt-in weaker behavior).
	Drained bool `json:"drained"`
	// Pending says the change was STAGED onto a sandbox that has not booted,
	// not applied to a running enforcer. Killed is necessarily 0 in that case:
	// a VM that has never run has no established flows to terminate.
	// Refs: MGIT-109, FR-17.10
	Pending bool `json:"pending,omitempty"`
}

// EgressChannelUnreachableError reports that the host could not reach whatever
// is enforcing a sandbox's egress policy, and carries the HOST-SIDE EVIDENCE a
// caller needs to say WHICH failure this is.
//
// It exists because one string used to serve three genuinely different
// failures as a two-cause guess — "the sandbox may not be running, or its VM
// predates this capability". Those are a sandbox whose VM has not booted
// (normal under lazy provisioning, FR-17.10), a VM that booted and died
// (MGIT-99), and a VM launched before the control channel existed (MGIT-74).
// Each has a different remedy, and collapsing them into a shrug is what sent a
// live bug report to two unrelated tickets (MGIT-102/103).
//
// The split of responsibility is deliberate: the enforcer side reports FACTS it
// can observe, and the service — which holds the recorded sandbox state — names
// the condition. Neither half can name it alone. Refs: MGIT-109, MGIT-104, SEC-04
type EgressChannelUnreachableError struct {
	// SocketPath is the control channel the host tried to reach.
	SocketPath string
	// VMStateSeen reports that this sandbox's host-side VM state directory
	// exists — i.e. a VM was launched for it at some point.
	VMStateSeen bool
	// ChannelSeen reports that the control socket itself exists on disk. A VM
	// that bound the channel leaves the socket behind when its process dies;
	// one that predates the channel never created it at all. That is what
	// separates "booted then died" from "predates this capability".
	ChannelSeen bool
	// Cause is the underlying dial failure.
	Cause error
}

// Error states only what was observed. The diagnosis is deliberately NOT here:
// this type does not know the sandbox's recorded state, and a guess written at
// this layer is the defect being fixed. Refs: MGIT-109
func (e *EgressChannelUnreachableError) Error() string {
	return fmt.Sprintf("vm control channel unreachable at %s: %v", e.SocketPath, e.Cause)
}

// Unwrap exposes the dial failure so callers can match on it.
func (e *EgressChannelUnreachableError) Unwrap() error { return e.Cause }

// Egress-policy failure codes: the STABLE, machine-readable vocabulary for why
// a policy verb (set / revoke / show) failed.
//
// THESE TOKENS ARE A CONTRACT. The prose beside them is not, and is expected to
// improve. An integrator built a pre-boot retry by matching on the error
// WORDING; it silently missed the not-booted failure entirely, and a later
// rewording would have broken it a second time just as silently. Matching on
// prose is now structurally unnecessary: match on the code.
//
// The set is CLOSED and every failure carries exactly one member of it. An
// unrecognized cause gets EgressFailureUnknown — never the nearest of the
// others, because a confident wrong answer is the defect this ticket exists to
// fix, one layer down. Refs: MGIT-109, R-H233, MGIT-104
const (
	// EgressFailureNotBooted: the sandbox is registered but its microVM has
	// never booted (lazy provisioning, FR-17.10). Nothing is enforcing egress
	// for it yet, and nothing needs to: the policy belongs on the pending
	// launch. Remedy: stage it (which `policy set` now does), or boot the VM.
	EgressFailureNotBooted = "NOT_BOOTED"
	// EgressFailureBootedDied: the sandbox is recorded as running, but the
	// enforcer is not answering — the guest has exited or was killed. Nothing
	// is enforcing egress for it NOW, which is not the same fact as the above.
	// Remedy: tear it down and relaunch.
	EgressFailureBootedDied = "BOOTED_DIED"
	// EgressFailureVersionPredates: the VM is running but was launched without
	// a control channel, by a build older than the live-policy capability
	// (MGIT-74). Its launch-time allowlist is still enforced and cannot be
	// changed in place. Remedy: relaunch it with this build.
	EgressFailureVersionPredates = "VERSION_PREDATES"
	// EgressFailureUnknown: a failure this build cannot classify. It exists so
	// the set can stay closed without ever having to guess.
	EgressFailureUnknown = "UNKNOWN"
)

// EgressPolicyError is a policy-verb failure carrying its stable Code
// alongside the human explanation.
//
// Code is what integrations branch on; Reason is for the person reading the
// terminal. Both travel: the code is rendered into the message text as well as
// exposed structurally, so a caller reading only stderr can still find it.
// Refs: MGIT-109, R-H233
type EgressPolicyError struct {
	// Code is one of the Egress* constants above. Never empty.
	Code string
	// Reason is the human explanation, naming the condition and its remedy.
	Reason string
	// Cause is the underlying failure, preserved for errors.Is/As.
	Cause error
}

// Error renders the code inline so the token is machine-readable even from a
// bare stderr line, not only from the structured field. Refs: MGIT-109, R-H233
func (e *EgressPolicyError) Error() string {
	if e.Cause == nil {
		return fmt.Sprintf("[%s] %s", e.Code, e.Reason)
	}
	return fmt.Sprintf("[%s] %s: %v", e.Code, e.Reason, e.Cause)
}

// Unwrap keeps the underlying failure matchable.
func (e *EgressPolicyError) Unwrap() error { return e.Cause }

// ValidEgressFailureCode reports whether a token is a member of the closed
// vocabulary. It is the guard that keeps an unrecognized code from being
// invented at a boundary rather than mapped to EgressFailureUnknown.
// Refs: MGIT-109, R-H233
func ValidEgressFailureCode(code string) bool {
	switch code {
	case EgressFailureNotBooted, EgressFailureBootedDied,
		EgressFailureVersionPredates, EgressFailureUnknown:
		return true
	default:
		return false
	}
}
