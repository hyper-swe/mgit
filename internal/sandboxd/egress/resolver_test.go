package egress

import (
	"context"
	"net/netip"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/hyper-swe/mgit/internal/model"
)

// fakeAuditor records every egress decision in memory.
type fakeAuditor struct {
	mu      sync.Mutex
	records []model.EgressRecord
}

func (a *fakeAuditor) AppendEgressRecord(_ context.Context, rec *model.EgressRecord) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.records = append(a.records, *rec)
	return nil
}

func (a *fakeAuditor) all() []model.EgressRecord {
	a.mu.Lock()
	defer a.mu.Unlock()
	out := make([]model.EgressRecord, len(a.records))
	copy(out, a.records)
	return out
}

func (a *fakeAuditor) withRule(substr string) int {
	n := 0
	for _, r := range a.all() {
		if substr == "" || contains(r.Rule, substr) {
			n++
		}
	}
	return n
}

func contains(s, sub string) bool {
	return len(sub) == 0 || (len(s) >= len(sub) && indexOf(s, sub) >= 0)
}
func indexOf(s, sub string) int {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return i
		}
	}
	return -1
}

func frozenClock() func() time.Time {
	t := time.Date(2026, 6, 21, 12, 0, 0, 0, time.UTC)
	return func() time.Time { return t }
}

func testResolver(t *testing.T, lookup LookupFunc, aud Auditor, clock func() time.Time) *Resolver {
	t.Helper()
	al, err := Compile([]string{"registry.npmjs.org", "*.golang.org"})
	require.NoError(t, err)
	r, err := NewResolver(ResolverConfig{
		SandboxID: "01SB", TaskID: "MGIT-11.7.3",
		Allowlist: al, Lookup: lookup, Audit: aud, Clock: clock,
		MaxQueriesPerWindow: 5, Window: time.Minute, NXDOMAINBurstThreshold: 3,
	})
	require.NoError(t, err)
	return r
}

// TestDNS_OnlyAllowlistedNamesResolve verifies the resolver resolves only
// allowlisted names; a non-allowlisted name is refused without ever
// invoking the upstream lookup (SEC-07). Refs: SEC-07, FR-17.8, MGIT-11.7.3
func TestDNS_OnlyAllowlistedNamesResolve(t *testing.T) {
	var lookups []string
	lookup := func(_ context.Context, name string) ([]netip.Addr, error) {
		lookups = append(lookups, name)
		return []netip.Addr{netip.MustParseAddr("140.82.112.3")}, nil
	}
	aud := &fakeAuditor{}
	r := testResolver(t, lookup, aud, frozenClock())

	ips, err := r.Resolve(context.Background(), "registry.npmjs.org")
	require.NoError(t, err)
	require.Len(t, ips, 1)
	assert.Equal(t, "140.82.112.3", ips[0].String())

	_, err = r.Resolve(context.Background(), "evil.example.com")
	assert.ErrorIs(t, err, ErrNameNotAllowlisted, "a non-allowlisted name must not resolve")
	assert.Equal(t, []string{"registry.npmjs.org"}, lookups,
		"the upstream resolver is never consulted for a non-allowlisted name (no label exfiltration)")
	assert.GreaterOrEqual(t, aud.withRule("not allowlisted"), 1, "the deny is audited")
}

// TestDNS_RateLimited verifies the per-sandbox query rate cap; queries
// beyond the window cap are refused (SEC-07). Refs: SEC-07, MGIT-11.7.3
func TestDNS_RateLimited(t *testing.T) {
	lookup := func(_ context.Context, _ string) ([]netip.Addr, error) {
		return []netip.Addr{netip.MustParseAddr("140.82.112.3")}, nil
	}
	aud := &fakeAuditor{}
	r := testResolver(t, lookup, aud, frozenClock()) // cap = 5 / minute, frozen time

	for i := 0; i < 5; i++ {
		_, err := r.Resolve(context.Background(), "registry.npmjs.org")
		require.NoError(t, err, "query %d within cap", i)
	}
	_, err := r.Resolve(context.Background(), "registry.npmjs.org")
	assert.ErrorIs(t, err, ErrRateLimited, "the 6th query in the window is rate-limited")
	assert.GreaterOrEqual(t, aud.withRule("rate"), 1, "the rate-limit deny is audited")
}

// TestDNS_RateLimited_WindowResets verifies the cap resets after the
// window elapses (the clock advances). Refs: SEC-07
func TestDNS_RateLimited_WindowResets(t *testing.T) {
	now := time.Date(2026, 6, 21, 12, 0, 0, 0, time.UTC)
	clock := func() time.Time { return now }
	lookup := func(_ context.Context, _ string) ([]netip.Addr, error) {
		return []netip.Addr{netip.MustParseAddr("140.82.112.3")}, nil
	}
	r := testResolver(t, lookup, &fakeAuditor{}, clock)
	for i := 0; i < 5; i++ {
		_, err := r.Resolve(context.Background(), "registry.npmjs.org")
		require.NoError(t, err)
	}
	now = now.Add(61 * time.Second) // next window
	_, err := r.Resolve(context.Background(), "registry.npmjs.org")
	assert.NoError(t, err, "the cap resets in the next window")
}

// TestDNS_NXDOMAINBurst_Flagged verifies a burst of NXDOMAIN responses is
// flagged in the audit log (a DNS-tunnel / subdomain-enumeration signal).
// Refs: SEC-07, MGIT-11.7.3
func TestDNS_NXDOMAINBurst_Flagged(t *testing.T) {
	lookup := func(_ context.Context, _ string) ([]netip.Addr, error) {
		return nil, ErrNXDOMAIN
	}
	aud := &fakeAuditor{}
	r := testResolver(t, lookup, aud, frozenClock()) // threshold = 3

	for i := 0; i < 3; i++ {
		_, err := r.Resolve(context.Background(), "registry.npmjs.org")
		assert.Error(t, err)
	}
	assert.True(t, r.NXDOMAINBurst(), "3 NXDOMAINs reach the burst threshold")
	assert.GreaterOrEqual(t, aud.withRule("nxdomain_burst"), 1, "the burst is flagged in the audit log")
}

// TestNewResolver_Validates rejects a missing dependency (DI contract).
func TestNewResolver_Validates(t *testing.T) {
	_, err := NewResolver(ResolverConfig{})
	assert.Error(t, err)
}
