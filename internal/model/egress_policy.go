package model

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
	RuleCount int `json:"rule_count"`
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
}
