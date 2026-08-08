package egress

import (
	"context"
	"io"
	"log/slog"
	"net/netip"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/hyper-swe/mgit/internal/model"
	"golang.org/x/net/dns/dnsmessage"
)

// resolvesToAll is resolvesTo for a multi-address answer, which the
// denied-IP-filtering assertion needs.
func resolvesToAll(ips ...string) LookupFunc {
	return func(context.Context, string) ([]netip.Addr, error) {
		out := make([]netip.Addr, 0, len(ips))
		for _, ip := range ips {
			out = append(out, netip.MustParseAddr(ip))
		}
		return out, nil
	}
}

// openResolverCfg is a resolver configured the way open mode configures one.
func openResolverCfg(t *testing.T, aud Auditor, lookup LookupFunc) ResolverConfig {
	t.Helper()
	return ResolverConfig{
		SandboxID:  "sbx-open",
		TaskID:     "MGIT-69",
		ResolveAny: true,
		Lookup:     lookup,
		Audit:      aud,
		Clock:      func() time.Time { return time.Unix(0, 0).UTC() },
		Logger:     slog.New(slog.NewTextHandler(io.Discard, nil)),
	}
}

// TestResolver_ResolveAny_ResolvesANameNoAllowlistNames verifies open mode's
// resolver answers a name no allowlist mentions.
//
// This exists because open mode bound NO resolver at all, while the guest was
// still told its nameserver was the gateway — so every name in open mode
// failed with "connection refused" against a port nothing listened on. A
// test that only dialed a RAW IP could not see it. Refs: MGIT-69, FR-17.7
func TestResolver_ResolveAny_ResolvesANameNoAllowlistNames(t *testing.T) {
	aud := &recordingAuditor{}
	r, err := NewResolver(openResolverCfg(t, aud, resolvesTo("140.82.112.3")))
	require.NoError(t, err)

	got, err := r.Resolve(context.Background(), "anything.example")

	require.NoError(t, err)
	assert.Equal(t, []netip.Addr{netip.MustParseAddr("140.82.112.3")}, got)
}

// TestResolver_ResolveAny_StillAudits verifies open-mode resolution is
// recorded. Open mode relaxes WHERE the guest may go, never whether mgit
// writes down where it went. Refs: FR-17.18, SEC-04
func TestResolver_ResolveAny_StillAudits(t *testing.T) {
	aud := &recordingAuditor{}
	r, err := NewResolver(openResolverCfg(t, aud, resolvesTo("140.82.112.3")))
	require.NoError(t, err)

	_, err = r.Resolve(context.Background(), "anything.example")
	require.NoError(t, err)

	recs := aud.snapshot()
	require.NotEmpty(t, recs, "open-mode DNS must still produce an audit record")
	assert.Equal(t, model.EgressAllow, recs[len(recs)-1].Decision)
}

// TestResolver_ResolveAny_StillDropsDeniedIPs verifies open mode does NOT
// relax the unconditional denials: a name resolving into loopback, RFC1918 or
// the metadata endpoint must not be answered with those addresses. Open means
// "any public destination", not "the host's LAN". Refs: SEC-04, T9
func TestResolver_ResolveAny_StillDropsDeniedIPs(t *testing.T) {
	r, err := NewResolver(openResolverCfg(t, &recordingAuditor{},
		resolvesToAll("127.0.0.1", "10.1.2.3", "169.254.169.254", "140.82.112.3")))
	require.NoError(t, err)

	got, err := r.Resolve(context.Background(), "rebind.example")

	require.NoError(t, err)
	assert.Equal(t, []netip.Addr{netip.MustParseAddr("140.82.112.3")}, got,
		"only the public address survives; open mode does not re-open denied ranges")
}

// TestResolver_ResolveAny_StillRateLimits verifies the SEC-07 anti-tunnel
// control still applies in open mode: DNS is a covert channel regardless of
// how permissive the egress policy is.
func TestResolver_ResolveAny_StillRateLimits(t *testing.T) {
	cfg := openResolverCfg(t, &recordingAuditor{}, resolvesTo("140.82.112.3"))
	cfg.MaxQueriesPerWindow = 2
	r, err := NewResolver(cfg)
	require.NoError(t, err)

	_, err1 := r.Resolve(context.Background(), "a.example")
	_, err2 := r.Resolve(context.Background(), "b.example")
	_, err3 := r.Resolve(context.Background(), "c.example")

	require.NoError(t, err1)
	require.NoError(t, err2)
	require.ErrorIs(t, err3, ErrRateLimited)
}

// TestResolver_AllowlistMode_Unchanged verifies the gate is untouched when
// ResolveAny is off — the SEC-07 property allowlist mode depends on.
func TestResolver_AllowlistMode_Unchanged(t *testing.T) {
	al, err := Compile([]string{"allowed.example:443"})
	require.NoError(t, err)
	cfg := openResolverCfg(t, &recordingAuditor{}, resolvesTo("140.82.112.3"))
	cfg.ResolveAny = false
	cfg.Allowlist = al
	r, err := NewResolver(cfg)
	require.NoError(t, err)

	_, okErr := r.Resolve(context.Background(), "allowed.example")
	_, denyErr := r.Resolve(context.Background(), "other.example")

	require.NoError(t, okErr)
	require.ErrorIs(t, denyErr, ErrNameNotAllowlisted)
}

// TestNewResolver_RequiresAnAllowlistUnlessResolveAny keeps the fail-closed
// construction rule: a resolver with neither a gate nor an explicit
// "resolve anything" is a misconfiguration, not a permissive default.
func TestNewResolver_RequiresAnAllowlistUnlessResolveAny(t *testing.T) {
	cfg := openResolverCfg(t, &recordingAuditor{}, resolvesTo("140.82.112.3"))
	cfg.ResolveAny = false
	cfg.Allowlist = nil

	_, err := NewResolver(cfg)

	require.Error(t, err)
	assert.Contains(t, err.Error(), "allowlist")
}

// TestNewOpenDNSServer_ServesOpenModeResolution verifies the one constructor
// both backends use, so open-mode DNS cannot be assembled two different ways.
// Refs: MGIT-69
func TestNewOpenDNSServer_ServesOpenModeResolution(t *testing.T) {
	srv, err := NewOpenDNSServer(OpenDNSConfig{
		SandboxID: "sbx-open",
		TaskID:    "MGIT-69",
		Audit:     &recordingAuditor{},
		Lookup:    resolvesTo("140.82.112.3"),
		Clock:     func() time.Time { return time.Unix(0, 0).UTC() },
		Logger:    slog.New(slog.NewTextHandler(io.Discard, nil)),
	})

	require.NoError(t, err)
	require.NotNil(t, srv)
	resp := srv.handleQuery(context.Background(), buildQuery(t, "anything.example", dnsmessage.TypeA))
	assert.NotEmpty(t, resp, "open mode must answer a name no allowlist names")
}

// TestNewOpenDNSServer_RequiresAnAuditor verifies open-mode DNS cannot be
// built without somewhere to write the record.
func TestNewOpenDNSServer_RequiresAnAuditor(t *testing.T) {
	_, err := NewOpenDNSServer(OpenDNSConfig{
		SandboxID: "sbx-open",
		Lookup:    resolvesTo("140.82.112.3"),
		Clock:     func() time.Time { return time.Unix(0, 0).UTC() },
	})

	require.Error(t, err)
}
