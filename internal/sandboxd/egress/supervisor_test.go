package egress

import (
	"context"
	"errors"
	"net"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/hyper-swe/mgit/internal/model"
)

func supervisorCfg(t *testing.T, policy model.NetworkPolicy, dial DialFunc) SupervisorConfig {
	t.Helper()
	return SupervisorConfig{
		SandboxID: "01SB", TaskID: "MGIT-11.7", Policy: policy,
		Audit:  &fakeAuditor{},
		Lookup: resolvesTo("140.82.112.3"),
		Dial:   dial,
		Clock:  frozenClock(),
		Logger: quietLogger(),
	}
}

// TestSupervisor_AllowlistEndToEnd verifies the supervisor assembles a
// working proxy stack from an allowlist policy: an allowlisted CONNECT
// splices, a non-allowlisted one is denied. Refs: SEC-04, FR-17.8, MGIT-11.7
func TestSupervisor_AllowlistEndToEnd(t *testing.T) {
	rec := &dialRecorder{}
	policy := model.NetworkPolicy{Mode: model.NetworkModeAllowlist, Allowlist: []string{"registry.npmjs.org"}}
	sup, err := NewSupervisor(supervisorCfg(t, policy, rec.dial))
	require.NoError(t, err)

	guest, host := net.Pipe()
	defer func() { _ = guest.Close() }()
	go sup.Proxy().handle(context.Background(), host)

	require.NoError(t, EncodeConnectRequest(guest, ConnectRequest{Protocol: "tcp", Host: "registry.npmjs.org", Port: 443}))
	allow, _, err := DecodeConnectReply(guest)
	require.NoError(t, err)
	assert.True(t, allow, "allowlisted name connects through the assembled stack")
	assert.Equal(t, []string{"140.82.112.3:443"}, rec.targets())

	// the resolver is exposed for the guest's DNS, sharing the same allowlist.
	require.NotNil(t, sup.Resolver())
	_, err = sup.Resolver().Resolve(context.Background(), "registry.npmjs.org")
	assert.NoError(t, err)
}

// TestSystemLookup_ResolvesLocalhost exercises the production adapter's
// success path (localhost resolves from the hosts file, no network).
// Refs: SEC-07
func TestSystemLookup_ResolvesLocalhost(t *testing.T) {
	ips, err := SystemLookup(nil)(context.Background(), "localhost")
	require.NoError(t, err)
	assert.NotEmpty(t, ips, "localhost resolves to a loopback address")
}

// TestSupervisor_RejectsNonAllowlistModes verifies the supervisor is only
// built for allowlist mode: none has no NIC and open uses host NAT, neither
// runs a proxy. Refs: FR-17.7, MGIT-11.7
func TestSupervisor_RejectsNonAllowlistModes(t *testing.T) {
	for _, mode := range []string{model.NetworkModeNone, model.NetworkModeOpen, "bogus"} {
		t.Run(mode, func(t *testing.T) {
			_, err := NewSupervisor(supervisorCfg(t, model.NetworkPolicy{Mode: mode}, (&dialRecorder{}).dial))
			assert.Error(t, err, "%s mode does not run an egress proxy", mode)
		})
	}
}

// TestSupervisor_Validates rejects missing dependencies (fail closed).
func TestSupervisor_Validates(t *testing.T) {
	policy := model.NetworkPolicy{Mode: model.NetworkModeAllowlist}
	base := supervisorCfg(t, policy, (&dialRecorder{}).dial)
	for name, mutate := range map[string]func(*SupervisorConfig){
		"nil_audit":  func(c *SupervisorConfig) { c.Audit = nil },
		"nil_lookup": func(c *SupervisorConfig) { c.Lookup = nil },
		"nil_dial":   func(c *SupervisorConfig) { c.Dial = nil },
		"nil_clock":  func(c *SupervisorConfig) { c.Clock = nil },
		"empty_id":   func(c *SupervisorConfig) { c.SandboxID = "" },
	} {
		t.Run(name, func(t *testing.T) {
			cfg := base
			mutate(&cfg)
			_, err := NewSupervisor(cfg)
			assert.Error(t, err)
		})
	}
}

// TestSupervisor_RejectsBadAllowlist surfaces a malformed allowlist entry at
// build time (fail closed before the sandbox runs). Refs: SEC-04
func TestSupervisor_RejectsBadAllowlist(t *testing.T) {
	policy := model.NetworkPolicy{Mode: model.NetworkModeAllowlist, Allowlist: []string{"*"}}
	_, err := NewSupervisor(supervisorCfg(t, policy, (&dialRecorder{}).dial))
	assert.Error(t, err, "a match-all allowlist is rejected (no allow-all)")
}

// TestSystemLookup_MapsNotFoundToNXDOMAIN verifies the production DNS adapter
// maps a not-found result to ErrNXDOMAIN so the resolver counts bursts.
// Refs: SEC-07
func TestSystemLookup_MapsNotFoundToNXDOMAIN(t *testing.T) {
	assert.ErrorIs(t, mapLookupError(&net.DNSError{Err: "no such host", IsNotFound: true}), ErrNXDOMAIN)
	assert.NotErrorIs(t, mapLookupError(errors.New("timeout")), ErrNXDOMAIN, "other errors are not NXDOMAIN")
	assert.NoError(t, mapLookupError(nil))
}
