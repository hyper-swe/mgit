package egress

import (
	"context"
	"io"
	"log/slog"
	"net/netip"
	"sync"
	"testing"

	"github.com/hyper-swe/mgit/internal/model"
)

// OPEN MODE. `open` deliberately places no restriction on destinations — the
// user asked for an unrestricted network. But "unrestricted" is not the same
// as "unobserved": the netstack gateway terminates every guest connection, so
// open mode can produce a per-flow AUDIT RECORD, which the iptables NAT path
// it replaces never could.
//
// It also cannot simply run with no authorizer: bindNetGateway refuses a nil
// one, correctly — a gateway with no policy object is indistinguishable from
// a bug that dropped the policy. Refs: SEC-04, FR-17.8, MGIT-61.9

// recordingAuditor captures the egress records an authorizer emits.
//
// It is mutex-guarded because the transparent proxy audits from its own
// per-connection goroutine while the test reads: the records are genuinely
// concurrent, not merely shared. Read them with snapshot().
type recordingAuditor struct {
	mu      sync.Mutex
	records []*model.EgressRecord
}

func (r *recordingAuditor) AppendEgressRecord(_ context.Context, rec *model.EgressRecord) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.records = append(r.records, rec)
	return nil
}

// snapshot returns a copy of the records recorded so far.
func (r *recordingAuditor) snapshot() []*model.EgressRecord {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]*model.EgressRecord, len(r.records))
	copy(out, r.records)
	return out
}

func testOpenAuthorizer(t *testing.T, aud Auditor) *OpenAuthorizer {
	t.Helper()
	a, err := NewOpenAuthorizer(OpenAuthorizerConfig{
		SandboxID: "sbx-open", TaskID: "MGIT-61.9",
		Audit:  aud,
		Logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
	})
	if err != nil {
		t.Fatalf("NewOpenAuthorizer: %v", err)
	}
	return a
}

func TestOpenAuthorizer_AllowsAnyDestinationAndAuditsIt(t *testing.T) {
	aud := &recordingAuditor{}
	a := testOpenAuthorizer(t, aud)

	dec, err := a.Authorize(context.Background(), Flow{Protocol: "tcp", Host: "93.184.216.34", Port: 443})
	if err != nil {
		t.Fatalf("Authorize: %v", err)
	}
	if !dec.Allow {
		t.Fatal("open mode must allow any destination — that is what the user asked for")
	}
	if dec.DestIP.String() != "93.184.216.34" {
		t.Errorf("DestIP = %v, want the requested address pinned for the dialer", dec.DestIP)
	}
	// The gain over iptables NAT: a per-flow record.
	if recs := aud.snapshot(); len(recs) != 1 {
		t.Fatalf("got %d audit records, want 1 — open mode must still be observable", len(recs))
	}
	rec := aud.snapshot()[0]
	if rec.Decision != model.EgressAllow || rec.DestIP != "93.184.216.34" || rec.DestPort != 443 {
		t.Errorf("audit record = %+v, want an allow for the requested destination", rec)
	}
	if rec.SandboxID != "sbx-open" || rec.TaskID != "MGIT-61.9" {
		t.Errorf("audit record is not attributed to the sandbox/task: %+v", rec)
	}
}

func TestOpenAuthorizer_StillRefusesWhatHasNoEgressPath(t *testing.T) {
	// Open means "any destination", NOT "any protocol". UDP has no egress
	// path in the gateway (only the DNS endpoint is bound), so admitting it
	// would promise something the data path cannot deliver.
	tests := []struct {
		name string
		flow Flow
	}{
		{name: "udp_has_no_egress_path", flow: Flow{Protocol: "udp", Host: "1.1.1.1", Port: 53}},
		{name: "port_below_range", flow: Flow{Protocol: "tcp", Host: "1.1.1.1", Port: 0}},
		{name: "port_above_range", flow: Flow{Protocol: "tcp", Host: "1.1.1.1", Port: 70000}},
		{name: "unresolvable_literal", flow: Flow{Protocol: "tcp", Host: "not-an-ip", Port: 443}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			aud := &recordingAuditor{}
			a := testOpenAuthorizer(t, aud)
			dec, err := a.Authorize(context.Background(), tt.flow)
			if dec.Allow || err == nil {
				t.Fatalf("open mode admitted %+v; it must refuse what the data path cannot carry", tt.flow)
			}
			if recs := aud.snapshot(); len(recs) != 1 || recs[0].Decision != model.EgressDeny {
				t.Errorf("the refusal must still be audited, got %+v", aud.snapshot())
			}
		})
	}
}

func TestOpenAuthorizer_RejectsIncompleteConfig(t *testing.T) {
	tests := []struct {
		name string
		cfg  OpenAuthorizerConfig
	}{
		{name: "no_auditor", cfg: OpenAuthorizerConfig{SandboxID: "s"}},
		{name: "no_sandbox_id", cfg: OpenAuthorizerConfig{Audit: &recordingAuditor{}}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if _, err := NewOpenAuthorizer(tt.cfg); err == nil {
				t.Fatal("an unattributable audit trail must not be constructible")
			}
		})
	}
}

func TestOpenAuthorizer_SatisfiesTheGatewaySeam(t *testing.T) {
	// The gateway holds a narrow interface; open mode must satisfy the same
	// one the allowlist authorizer does, or it cannot be wired at all.
	var _ interface {
		Authorize(context.Context, Flow) (Decision, error)
	} = testOpenAuthorizer(t, &recordingAuditor{})

	var _ netip.Addr // keep the import honest about what DestIP is
}
