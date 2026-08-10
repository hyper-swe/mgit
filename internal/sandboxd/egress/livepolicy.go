package egress

import (
	"fmt"
	"net"
	"sync"
)

// SetRules atomically replaces the compiled allowlist of a RUNNING sandbox.
//
// This is the enforcement half of a live policy change (MGIT-72). It is
// tractable because the authorizer is consulted PER CONNECTION: once the rules
// are swapped, the very next flow is decided against the new policy with no VM
// involvement at all.
//
// ATOMICITY. The replacement is compiled BEFORE the lock is taken and swapped
// in one assignment, so a concurrent decision sees the old ruleset or the new
// one and never a mixture. A policy that does not compile is rejected with the
// running one untouched — a half-applied policy is the one outcome a caller
// can neither predict nor audit.
//
// LIVE GRANTS ARE DROPPED. A capability grant (FR-17.12) widens the policy it
// was approved under; carrying it across a policy change would leave a hole
// that is invisible in the new policy. A caller who still wants it re-grants
// it against the new rules. Refs: MGIT-72, SEC-04, SEC-05, ADR-012
func (al *Allowlist) SetRules(entries []string) error {
	next, err := Compile(entries)
	if err != nil {
		return fmt.Errorf("egress policy: %w", err)
	}
	al.mu.Lock()
	defer al.mu.Unlock()
	// source moves with the rules: a policy that reported the entries it was
	// launched with after being mutated would be worse than reporting nothing.
	al.names, al.nets, al.source = next.names, next.nets, next.source
	al.grants = nil
	return nil
}

// Entries reports the compiled rule count, so an audit record can state what a
// policy change actually produced rather than only what was asked for.
func (al *Allowlist) Entries() int {
	al.mu.RLock()
	defer al.mu.RUnlock()
	return len(al.names) + len(al.nets)
}

// FlowRegistry tracks the live spliced connections of one sandbox so a policy
// revoke can KILL them.
//
// WHY KILL IS THE DEFAULT (ADR-012). A caller who revokes package-registry
// egress and then runs untrusted code expects the grant to be gone. A draining
// connection is exactly the exfiltration channel they just revoked, and a
// hostile guest can hold one open arbitrarily long — so "drain" can mean
// "never". Draining remains available, but it has to be asked for by name.
//
// The zero value is not usable; use NewFlowRegistry. A NIL registry is a
// working no-op, so a data path that has no registry needs no branch.
type FlowRegistry struct {
	mu    sync.Mutex
	conns map[*flowPair]struct{}
}

// flowPair is one tracked flow: the guest side and the destination side, which
// are killed together because half a connection is not a flow.
type flowPair struct{ a, b net.Conn }

// NewFlowRegistry returns an empty registry.
func NewFlowRegistry() *FlowRegistry {
	return &FlowRegistry{conns: make(map[*flowPair]struct{})}
}

// Track registers a live flow and returns the release to call when it ends.
// The release is idempotent and safe after CloseAll.
func (r *FlowRegistry) Track(a, b net.Conn) func() {
	if r == nil {
		return func() {}
	}
	pair := &flowPair{a: a, b: b}
	r.mu.Lock()
	r.conns[pair] = struct{}{}
	r.mu.Unlock()
	var once sync.Once
	return func() {
		once.Do(func() {
			r.mu.Lock()
			delete(r.conns, pair)
			r.mu.Unlock()
		})
	}
}

// CloseAll kills every tracked flow and returns how many were killed. The
// count is reported so the audit record can state what a revoke actually
// terminated, not merely that it was asked to.
func (r *FlowRegistry) CloseAll() int {
	if r == nil {
		return 0
	}
	r.mu.Lock()
	pairs := make([]*flowPair, 0, len(r.conns))
	for p := range r.conns {
		pairs = append(pairs, p)
	}
	r.conns = make(map[*flowPair]struct{})
	r.mu.Unlock()

	for _, p := range pairs {
		// Both halves: closing only the guest side would leave the host-side
		// connection to the destination established.
		_ = p.a.Close()
		_ = p.b.Close()
	}
	return len(pairs)
}

// Len reports how many flows are currently tracked.
func (r *FlowRegistry) Len() int {
	if r == nil {
		return 0
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.conns)
}

// SpliceTracked is Splice with the flow registered for the duration, so a
// policy revoke can reach an established connection. A nil registry makes it
// exactly Splice. Refs: MGIT-72
func SpliceTracked(reg *FlowRegistry, guest, outbound net.Conn) {
	release := reg.Track(guest, outbound)
	defer release()
	Splice(guest, outbound)
}

// PolicyChange reports what a live policy mutation actually did, so the caller
// and the audit record state outcomes rather than intentions.
type PolicyChange struct {
	SandboxID string
	TaskID    string
	Entries   []string // the policy now in force
	RuleCount int      // rules the new policy compiled to
	Killed    int      // established flows terminated (0 when draining)
	Drained   bool     // established flows were left to finish
}

// PolicyState is the egress policy a RUNNING sandbox is enforcing right now.
// It is deliberately distinct from the launch-time policy on SandboxInfo: once
// a live mutation has happened those two disagree, and reporting the launch
// one would tell a caller egress is open when it is closed, or closed when it
// is open. Refs: MGIT-72
type PolicyState struct {
	SandboxID string
	Entries   []string
	RuleCount int
}

// Policy reports what a RUNNING sandbox's egress stack is enforcing.
//
// It fails closed on an unknown sandbox rather than returning an empty policy:
// "nothing is allowed" and "nothing is enforcing" look identical in an empty
// list, and they are opposite facts. Refs: MGIT-72, SEC-04
func (r *Runner) Policy(sandboxID string) (PolicyState, error) {
	r.mu.Lock()
	ae, ok := r.active[sandboxID]
	r.mu.Unlock()
	if !ok {
		return PolicyState{}, fmt.Errorf(
			"egress runner: policy: sandbox %q has no running egress stack", sandboxID)
	}
	al := ae.sup.Allowlist()
	return PolicyState{SandboxID: sandboxID, Entries: al.Rules(), RuleCount: al.Entries()}, nil
}

// SetPolicy replaces a RUNNING sandbox's egress allowlist.
//
// ESTABLISHED FLOWS ARE KILLED unless drain is set. That is the decision
// recorded in ADR-012 and it is deliberate: a caller who revokes
// package-registry egress and then runs untrusted code expects the grant to be
// GONE, and a draining connection is precisely the exfiltration channel they
// just revoked — a hostile guest can hold one open arbitrarily long, so
// "drain" can mean "never". The weaker behavior remains available, but it has
// to be asked for by name.
//
// AUTHORITY: this is a HOST-side entry point on the daemon's runner. Nothing
// reachable from the guest calls it — the guest's only channels are exec,
// land and the egress data path, none of which mutate policy (SEC-05).
//
// It fails closed on an unknown or proxy-less sandbox rather than silently
// doing nothing, since "revoke succeeded" for a sandbox that was never
// enforcing would be the most dangerous possible lie. Refs: MGIT-72, SEC-04, SEC-05
func (r *Runner) SetPolicy(sandboxID string, entries []string, drain bool) (PolicyChange, error) {
	r.mu.Lock()
	ae, ok := r.active[sandboxID]
	r.mu.Unlock()
	if !ok {
		return PolicyChange{}, fmt.Errorf(
			"egress runner: set policy: sandbox %q has no running egress stack", sandboxID)
	}

	if err := ae.sup.Allowlist().SetRules(entries); err != nil {
		// The running policy is untouched; nothing partial was applied.
		return PolicyChange{}, fmt.Errorf("egress runner: set policy: %w", err)
	}
	change := PolicyChange{
		SandboxID: sandboxID,
		Entries:   append([]string(nil), entries...),
		RuleCount: ae.sup.Allowlist().Entries(),
		Drained:   drain,
	}
	if !drain {
		change.Killed = ae.sup.Flows().CloseAll()
	}
	// Audited unconditionally: a policy that can change at runtime without a
	// record is worse than one that cannot change at all, because a reviewer
	// can no longer reconstruct what was enforced when. Refs: FR-17.18
	r.cfg.Logger.Info("sandbox egress policy changed",
		"event", "egress_policy_set", "sandbox_id", sandboxID,
		"entries", change.Entries, "rule_count", change.RuleCount,
		"established_flows_killed", change.Killed, "drained", drain)
	return change, nil
}
