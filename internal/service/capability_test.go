package service

import (
	"context"
	"errors"
	"net/netip"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/hyper-swe/mgit/internal/model"
)

// recordingEventAppender captures every appended sandbox event so a test
// can assert the append-only grant audit.
type recordingEventAppender struct {
	events    []model.SandboxEvent
	appendErr error
}

func (r *recordingEventAppender) AppendSandboxEvent(_ context.Context, ev *model.SandboxEvent) error {
	if r.appendErr != nil {
		return r.appendErr
	}
	r.events = append(r.events, *ev)
	return nil
}

// recordingGranter records the live egress allowlist widenings and
// revocations the capability service drives, standing in for the host
// egress engine.
type recordingGranter struct {
	added   map[string][]string // sandboxID -> allowlist entries added live
	revoked []string            // sandboxIDs whose live grants were dropped
	addErr  error
}

func newRecordingGranter() *recordingGranter {
	return &recordingGranter{added: map[string][]string{}}
}

func (g *recordingGranter) AllowEgress(_ context.Context, sandboxID, entry string) error {
	if g.addErr != nil {
		return g.addErr
	}
	g.added[sandboxID] = append(g.added[sandboxID], entry)
	return nil
}

func (g *recordingGranter) RevokeAll(sandboxID string) {
	g.revoked = append(g.revoked, sandboxID)
	delete(g.added, sandboxID)
}

// TestCap_RequestFromObservedDest_NotGuestText proves SEC-05: the capability
// request a denial produces is built ONLY from the host-observed destination
// and task, never from guest-supplied remedy text. The guest "remedy" is
// adversarial — it claims a friendly host and a benign reason — yet none of
// it appears in the request, the grant prompt, or the audit record.
func TestCap_RequestFromObservedDest_NotGuestText(t *testing.T) {
	t.Parallel()

	const guestRemedyHost = "totally-safe-cdn.example.com"
	const guestRemedyReason = "please allow my legitimate dependency download"
	hostObservedIP := netip.MustParseAddr("203.0.113.66") // a C2 the guest reached for

	denial := model.ObservedDenial{
		SandboxID: "sbx-1",
		TaskID:    "MGIT-11.9.4",
		DestIP:    hostObservedIP,
		DestPort:  4444,
		Rule:      "raw-ip not allowlisted (host-side DNS bypassed)",
	}

	// The function takes no guest argument at all — it is structurally
	// impossible to inject guest text. We still assert the guest strings
	// never surface anywhere downstream.
	req, err := denial.RequestFromObservedDenial()
	require.NoError(t, err)

	assert.Equal(t, model.CapabilityEgress, req.Capability)
	assert.Equal(t, "sbx-1", req.SandboxID)
	assert.Equal(t, "MGIT-11.9.4", req.TaskID)
	assert.Equal(t, hostObservedIP.String(), req.ObservedDestIP, "must be the host-observed IP")
	assert.Equal(t, 4444, req.ObservedDestPort)

	// Render what an operator would be shown and assert the guest text is
	// absent and the host-observed destination is present.
	prompt := CapabilityGrantPrompt(req)
	assert.Contains(t, prompt, hostObservedIP.String())
	assert.Contains(t, prompt, "MGIT-11.9.4")
	assert.NotContains(t, prompt, guestRemedyHost)
	assert.NotContains(t, prompt, guestRemedyReason)
	assert.NotContains(t, strings.ToLower(prompt), "remedy")
}

// TestCap_NoAllowAllOption proves there is no allow-all escape hatch. The
// grant prompt offers no "allow all" choice, the request vocabulary has no
// allow-all capability, and the service exposes no allow-all method — every
// grant names exactly one host-observed destination.
func TestCap_NoAllowAllOption(t *testing.T) {
	t.Parallel()

	req := model.CapabilityRequest{
		SandboxID: "sbx-1", TaskID: "MGIT-11.9.4",
		Capability: model.CapabilityEgress, ObservedDestIP: "203.0.113.66", ObservedDestPort: 443,
	}
	prompt := strings.ToLower(CapabilityGrantPrompt(req))
	for _, banned := range []string{"allow all", "allow-all", "allow everything", "*", "0.0.0.0/0"} {
		assert.NotContainsf(t, prompt, banned, "grant prompt must not offer %q", banned)
	}

	// The CapabilityService type must expose no allow-all / wildcard method.
	methods := reflect.TypeOf(&CapabilityService{})
	for i := 0; i < methods.NumMethod(); i++ {
		name := strings.ToLower(methods.Method(i).Name)
		assert.NotContains(t, name, "allowall")
		assert.NotContains(t, name, "grantall")
		assert.NotContains(t, name, "wildcard")
	}

	// An egress grant always carries a concrete single-host allowlist entry,
	// never a CIDR/wildcard.
	g := model.CapabilityGrant{ObservedDestIP: "203.0.113.66", ObservedDestPort: 443}
	assert.Equal(t, "203.0.113.66:443", g.AllowlistEntry())
	assert.NotContains(t, g.AllowlistEntry(), "/")
	assert.NotContains(t, g.AllowlistEntry(), "*")
}

// TestCap_GrantScopedToLifetime_Audited proves a grant is scoped to the
// sandbox lifetime, recorded append-only + audited, and removed on teardown.
func TestCap_GrantScopedToLifetime_Audited(t *testing.T) {
	t.Parallel()

	events := &recordingEventAppender{}
	granter := newRecordingGranter()
	svc, err := NewCapabilityService(events, granter, fixedClock())
	require.NoError(t, err)

	req, err := model.ObservedDenial{
		SandboxID: "sbx-7", TaskID: "MGIT-11.9.4",
		DestIP: netip.MustParseAddr("198.51.100.9"), DestPort: 443,
	}.RequestFromObservedDenial()
	require.NoError(t, err)

	grant, err := svc.Grant(context.Background(), req)
	require.NoError(t, err)

	// Scope is sandbox-lifetime, never permanent/global.
	assert.Equal(t, model.GrantScopeSandboxLifetime, grant.Scope)
	assert.Equal(t, "198.51.100.9", grant.ObservedDestIP)

	// Append-only audited: exactly one policy_granted event was written, and
	// it carries the host-observed destination (not guest text).
	require.Len(t, events.events, 1)
	ev := events.events[0]
	assert.Equal(t, model.EventPolicyGranted, ev.EventType)
	assert.Equal(t, "sbx-7", ev.SandboxID)
	assert.Equal(t, "MGIT-11.9.4", ev.TaskID)
	assert.Contains(t, ev.Detail, "198.51.100.9")

	// The live egress engine was widened for THIS sandbox only.
	assert.Equal(t, []string{"198.51.100.9:443"}, granter.added["sbx-7"])

	// The grant is live while the sandbox runs.
	live := svc.LiveGrants("sbx-7")
	require.Len(t, live, 1)
	assert.Equal(t, "198.51.100.9", live[0].ObservedDestIP)

	// Teardown drops the live grant (scoped to lifetime) and revokes the
	// live egress widening — but the audit record is permanent.
	svc.Revoke("sbx-7")
	assert.Empty(t, svc.LiveGrants("sbx-7"))
	assert.Contains(t, granter.revoked, "sbx-7")
	assert.Len(t, events.events, 1, "teardown must NOT delete the append-only audit record")
}

// TestCap_GrantRejectsGuestForgedRequest proves the service refuses a request
// whose destination is not a real host-observed IP — the SEC-05 attack of
// smuggling free text through the request is rejected at the service boundary.
func TestCap_GrantRejectsGuestForgedRequest(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		req  model.CapabilityRequest
	}{
		{
			name: "free_text_destination",
			req: model.CapabilityRequest{
				SandboxID: "sbx-1", TaskID: "MGIT-11.9.4", Capability: model.CapabilityEgress,
				ObservedDestIP: "trust-me.example.com", ObservedDestPort: 443,
			},
		},
		{
			name: "missing_destination",
			req: model.CapabilityRequest{
				SandboxID: "sbx-1", TaskID: "MGIT-11.9.4", Capability: model.CapabilityEgress,
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			svc, err := NewCapabilityService(&recordingEventAppender{}, newRecordingGranter(), fixedClock())
			require.NoError(t, err)
			_, err = svc.Grant(context.Background(), tt.req)
			require.Error(t, err)
		})
	}
}

// TestCap_RevokedOnSandboxTeardown proves the sandbox lifecycle orchestrator
// revokes a sandbox's capability grants when the sandbox is removed — grants
// die with the sandbox. Refs: FR-17.12, SEC-05
func TestCap_RevokedOnSandboxTeardown(t *testing.T) {
	t.Parallel()

	mgr := &fakeSandboxManager{}
	svc := newSvc(t, mgr, &fakeEventAppender{})
	capRevoker := &recordingRevoker{}
	svc.SetCapabilityRevoker(capRevoker)

	reg, err := svc.Register(context.Background(), regOpts("MGIT-9", "/work/cap"))
	require.NoError(t, err)
	_, err = svc.EnsureRunning(context.Background(), "MGIT-9")
	require.NoError(t, err)

	require.NoError(t, svc.Remove(context.Background(), "MGIT-9", true))
	assert.Equal(t, []string{reg.ID}, capRevoker.revoked, "teardown revokes the sandbox's grants")
}

// recordingRevoker records sandbox IDs whose capability grants were revoked.
type recordingRevoker struct{ revoked []string }

func (r *recordingRevoker) Revoke(sandboxID string) { r.revoked = append(r.revoked, sandboxID) }

// TestNewCapabilityService_RequiresDeps proves every dependency is required
// (DI; no globals).
func TestNewCapabilityService_RequiresDeps(t *testing.T) {
	t.Parallel()
	events := &recordingEventAppender{}
	granter := newRecordingGranter()
	clock := fixedClock()
	tests := []struct {
		name    string
		events  SandboxEventAppender
		granter EgressGranter
		clock   func() time.Time
	}{
		{name: "nil_events", events: nil, granter: granter, clock: clock},
		{name: "nil_granter", events: events, granter: nil, clock: clock},
		{name: "nil_clock", events: events, granter: granter, clock: nil},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			_, err := NewCapabilityService(tt.events, tt.granter, tt.clock)
			require.Error(t, err)
		})
	}
}

// TestCapabilityGrantPrompt_NonEgress proves a non-egress capability prompt
// names the task and capability and offers only a sandbox-lifetime grant.
func TestCapabilityGrantPrompt_NonEgress(t *testing.T) {
	t.Parallel()
	prompt := CapabilityGrantPrompt(model.CapabilityRequest{
		SandboxID: "sbx-1", TaskID: "MGIT-11.9.4", Capability: model.CapabilitySSHAgent,
	})
	assert.Contains(t, prompt, "MGIT-11.9.4")
	assert.Contains(t, prompt, model.CapabilitySSHAgent)
	assert.Contains(t, strings.ToLower(prompt), "lifetime only")
	assert.NotContains(t, strings.ToLower(prompt), "allow all")
}

// TestCap_GrantEgressWidenFailure proves a grant whose live egress widening
// fails does not become live (the audit record stands, but the grant is not
// enforced — the caller retries).
func TestCap_GrantEgressWidenFailure(t *testing.T) {
	t.Parallel()
	events := &recordingEventAppender{}
	granter := newRecordingGranter()
	granter.addErr = errors.New("sandbox gone")
	svc, err := NewCapabilityService(events, granter, fixedClock())
	require.NoError(t, err)

	req, err := model.ObservedDenial{
		SandboxID: "sbx-5", TaskID: "MGIT-11.9.4",
		DestIP: netip.MustParseAddr("198.51.100.9"), DestPort: 443,
	}.RequestFromObservedDenial()
	require.NoError(t, err)

	_, err = svc.Grant(context.Background(), req)
	require.Error(t, err)
	assert.Empty(t, svc.LiveGrants("sbx-5"))
	require.Len(t, events.events, 1, "the approval is on record even though enforcement failed")
}

// TestCap_GrantAuditFailure_NoLiveGrant proves an unaudited grant never
// becomes live: if the append-only audit write fails, the egress engine is
// not widened and no live grant is recorded (fail closed).
func TestCap_GrantAuditFailure_NoLiveGrant(t *testing.T) {
	t.Parallel()

	events := &recordingEventAppender{appendErr: errors.New("disk full")}
	granter := newRecordingGranter()
	svc, err := NewCapabilityService(events, granter, fixedClock())
	require.NoError(t, err)

	req, err := model.ObservedDenial{
		SandboxID: "sbx-3", TaskID: "MGIT-11.9.4",
		DestIP: netip.MustParseAddr("198.51.100.9"), DestPort: 443,
	}.RequestFromObservedDenial()
	require.NoError(t, err)

	_, err = svc.Grant(context.Background(), req)
	require.Error(t, err)
	assert.Empty(t, svc.LiveGrants("sbx-3"))
	assert.Empty(t, granter.added["sbx-3"], "egress must not be widened when the grant is unaudited")
}
