package service

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/hyper-swe/mgit/internal/model"
)

// EgressGranter widens a LIVE sandbox's egress allowlist with one concrete
// host:port and drops every live widening on teardown. It is the seam to the
// host egress engine (the running allowlist proxy); the capability service
// never persists a widening to the launch policy — a grant lives only as long
// as the sandbox (scoped to the sandbox lifetime). Implementations must admit
// only the exact entry given (no CIDR/wildcard expansion). Refs: FR-17.12, SEC-05
type EgressGranter interface {
	// AllowEgress admits one host:port for the named live sandbox.
	AllowEgress(ctx context.Context, sandboxID, entry string) error
	// RevokeAll drops every live egress grant for the sandbox (teardown).
	RevokeAll(sandboxID string)
}

// CapabilityService approves boundary-crossing capability requests for a
// running sandbox. It is the escalation engine: a denial observed by the host
// egress engine produces a CapabilityRequest built ONLY from host-observed
// facts (model.ObservedDenial, SEC-05); an operator approval is turned into a
// grant that is (1) recorded append-only as a policy_granted sandbox event,
// (2) held live only while the sandbox runs, and (3) reflected into the live
// egress allowlist. There is deliberately NO allow-all path: every grant names
// the one host-observed destination it authorizes. Refs: FR-17.12, FR-17.18, SEC-05
type CapabilityService struct {
	events  SandboxEventAppender
	granter EgressGranter
	clock   func() time.Time

	// live holds grants for running sandboxes, keyed by sandbox ID. It is
	// in-memory by design: a grant is scoped to the sandbox lifetime, so it
	// must die when the sandbox (and this daemon's view of it) does. The
	// DURABLE record is the append-only policy_granted event; this map is
	// only the live enforcement view.
	mu   sync.Mutex
	live map[string][]model.CapabilityGrant
}

// NewCapabilityService wires the escalation engine. All dependencies are
// required (DI; no globals); clock is injected for deterministic audit
// timestamps. Refs: FR-17.12
func NewCapabilityService(events SandboxEventAppender, granter EgressGranter, clock func() time.Time) (*CapabilityService, error) {
	switch {
	case events == nil:
		return nil, fmt.Errorf("capability service: event appender must not be nil")
	case granter == nil:
		return nil, fmt.Errorf("capability service: egress granter must not be nil")
	case clock == nil:
		return nil, fmt.Errorf("capability service: clock must not be nil")
	}
	return &CapabilityService{
		events: events, granter: granter, clock: clock,
		live: make(map[string][]model.CapabilityGrant),
	}, nil
}

// Grant approves one capability request and returns the recorded grant. The
// request must already be host-derived (model.ObservedDenial.RequestFromObservedDenial);
// Grant re-validates it so a forged free-text destination is refused at the
// service boundary (SEC-05). Order is fail-closed: the append-only audit
// record is written FIRST — an unaudited grant must never take effect (an
// audit-trail gap, FR-17.18) — then the live egress allowlist is widened, then
// the grant is held live for the sandbox's lifetime. Refs: FR-17.12, FR-17.18, SEC-05
func (s *CapabilityService) Grant(ctx context.Context, req model.CapabilityRequest) (*model.CapabilityGrant, error) {
	if err := req.Validate(); err != nil {
		return nil, fmt.Errorf("capability grant: %w", err)
	}
	grant := model.CapabilityGrant{
		SandboxID:        req.SandboxID,
		TaskID:           req.TaskID,
		Capability:       req.Capability,
		ObservedDestIP:   req.ObservedDestIP,
		ObservedDestPort: req.ObservedDestPort,
		Scope:            model.GrantScopeSandboxLifetime,
		GrantedAt:        s.clock().UTC(),
	}

	// (1) Append-only audit FIRST. The detail records the host-observed
	// destination and the sandbox-lifetime scope — never guest text (SEC-05).
	if err := s.events.AppendSandboxEvent(ctx, grantEvent(grant)); err != nil {
		return nil, fmt.Errorf("capability grant: audit: %w", err)
	}

	// (2) Widen the LIVE egress allowlist for this sandbox only. A failure
	// here leaves the audit record (the grant was approved and is on record)
	// but does not make the grant live — the caller retries.
	if req.Capability == model.CapabilityEgress {
		if err := s.granter.AllowEgress(ctx, req.SandboxID, grant.AllowlistEntry()); err != nil {
			return nil, fmt.Errorf("capability grant: widen egress: %w", err)
		}
	}

	// (3) Hold it live for the sandbox lifetime.
	s.mu.Lock()
	s.live[req.SandboxID] = append(s.live[req.SandboxID], grant)
	s.mu.Unlock()
	return &grant, nil
}

// LiveGrants returns the capability grants currently in force for a running
// sandbox (a copy; the caller may not mutate the registry). After Revoke
// (teardown) the result is empty — grants are scoped to the sandbox lifetime.
// Refs: FR-17.12
func (s *CapabilityService) LiveGrants(sandboxID string) []model.CapabilityGrant {
	s.mu.Lock()
	defer s.mu.Unlock()
	grants := s.live[sandboxID]
	if len(grants) == 0 {
		return nil
	}
	out := make([]model.CapabilityGrant, len(grants))
	copy(out, grants)
	return out
}

// Revoke drops every live grant for a sandbox and tells the egress engine to
// stop honoring them. Called on teardown so a grant never outlives its
// sandbox. The append-only audit records are NOT touched — they are permanent.
// Refs: FR-17.12, FR-17.18, SEC-05
func (s *CapabilityService) Revoke(sandboxID string) {
	s.mu.Lock()
	delete(s.live, sandboxID)
	s.mu.Unlock()
	s.granter.RevokeAll(sandboxID)
}

// grantEvent renders a capability grant as an append-only policy_granted
// sandbox event. The detail JSON carries the host-observed destination and the
// sandbox-lifetime scope so the audit trail shows the real destination, never
// guest text (SEC-05). Refs: FR-17.12, FR-17.18
func grantEvent(g model.CapabilityGrant) *model.SandboxEvent {
	detail := fmt.Sprintf(
		`{"capability":%q,"scope":%q,"observed_dest_ip":%q,"observed_dest_port":%d}`,
		g.Capability, g.Scope, g.ObservedDestIP, g.ObservedDestPort)
	return &model.SandboxEvent{
		SandboxID: g.SandboxID,
		TaskID:    g.TaskID,
		EventType: model.EventPolicyGranted,
		Detail:    detail,
	}
}

// CapabilityGrantPrompt renders the human approval prompt for a capability
// request. It shows ONLY host-observed facts — the requesting task and the
// real destination the host saw — and offers a single scoped (sandbox-lifetime)
// approval. It never echoes guest-supplied text and never offers an allow-all
// option (SEC-05). Refs: FR-17.12, SEC-05
func CapabilityGrantPrompt(req model.CapabilityRequest) string {
	switch req.Capability {
	case model.CapabilityEgress:
		return fmt.Sprintf(
			"Task %s (sandbox %s) was denied egress to %s:%d (host-observed).\n"+
				"Grant egress to %s:%d for this sandbox's lifetime only? [y/N]",
			req.TaskID, req.SandboxID, req.ObservedDestIP, req.ObservedDestPort,
			req.ObservedDestIP, req.ObservedDestPort)
	default:
		return fmt.Sprintf(
			"Task %s (sandbox %s) requests the %q capability.\n"+
				"Grant it for this sandbox's lifetime only? [y/N]",
			req.TaskID, req.SandboxID, req.Capability)
	}
}
