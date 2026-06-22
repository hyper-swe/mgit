package egress

import (
	"context"
	"net"
	"net/netip"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"golang.org/x/net/dns/dnsmessage"

	"github.com/hyper-swe/mgit/internal/model"
)

func testRunner(t *testing.T, rec *dialRecorder) *Runner {
	t.Helper()
	r, err := NewRunner(RunnerConfig{
		Audit:  &fakeAuditor{},
		Lookup: resolvesTo("140.82.112.3"),
		Dial:   rec.dial,
		Clock:  frozenClock(),
		Logger: quietLogger(),
		// port 0 => ephemeral host ports for the test (prod uses fixed ports
		// the tap firewall references).
		ProxyPort: 0,
		DNSPort:   0,
	})
	require.NoError(t, err)
	return r
}

func allowlistPolicy(hosts ...string) model.NetworkPolicy {
	return model.NetworkPolicy{Mode: model.NetworkModeAllowlist, Allowlist: hosts}
}

// TestRunner_Start_RunsProxyAndDNS verifies Start brings up both the egress
// proxy and the DNS server for an allowlist sandbox, bound to the gateway,
// and that they enforce the policy end to end. Refs: SEC-04, SEC-07, MGIT-11.7
func TestRunner_Start_RunsProxyAndDNS(t *testing.T) {
	rec := &dialRecorder{}
	r := testRunner(t, rec)
	gw := netip.MustParseAddr("127.0.0.1")

	ep, err := r.Start(context.Background(), Binding{
		SandboxID: "01SB", TaskID: "MGIT-11.7", GatewayIP: gw,
		Policy: allowlistPolicy("registry.npmjs.org"),
	})
	require.NoError(t, err)
	defer func() { _ = r.Stop("01SB") }()
	require.NotEmpty(t, ep.ProxyAddr)
	require.NotEmpty(t, ep.DNSAddr)

	// DNS: an allowlisted name resolves.
	dnsConn, err := net.Dial("udp", ep.DNSAddr)
	require.NoError(t, err)
	defer func() { _ = dnsConn.Close() }()
	_, err = dnsConn.Write(dnsQueryA(t, "registry.npmjs.org"))
	require.NoError(t, err)
	_ = dnsConn.SetReadDeadline(time.Now().Add(2 * time.Second))
	buf := make([]byte, 512)
	n, err := dnsConn.Read(buf)
	require.NoError(t, err)
	var p dnsmessage.Parser
	hdr, err := p.Start(buf[:n])
	require.NoError(t, err)
	assert.Equal(t, dnsmessage.RCodeSuccess, hdr.RCode)

	// Proxy: an allowlisted CONNECT splices to the host-dialed pinned IP.
	proxyConn, err := net.Dial("tcp", ep.ProxyAddr)
	require.NoError(t, err)
	defer func() { _ = proxyConn.Close() }()
	require.NoError(t, EncodeConnectRequest(proxyConn, ConnectRequest{Protocol: "tcp", Host: "registry.npmjs.org", Port: 443}))
	allow, _, err := DecodeConnectReply(proxyConn)
	require.NoError(t, err)
	assert.True(t, allow, "allowlisted flow admitted through the running proxy")
}

// TestRunner_Start_NoneAndOpen_NoListeners verifies the runner starts no
// proxy for none/open modes (no proxy needed). Refs: FR-17.7
func TestRunner_Start_NoneAndOpen_NoListeners(t *testing.T) {
	for _, mode := range []string{model.NetworkModeNone, model.NetworkModeOpen} {
		t.Run(mode, func(t *testing.T) {
			r := testRunner(t, &dialRecorder{})
			ep, err := r.Start(context.Background(), Binding{
				SandboxID: "01SB", TaskID: "MGIT-11.7",
				GatewayIP: netip.MustParseAddr("127.0.0.1"),
				Policy:    model.NetworkPolicy{Mode: mode},
			})
			require.NoError(t, err)
			assert.Empty(t, ep.ProxyAddr, "%s mode runs no egress proxy", mode)
			assert.False(t, r.Running("01SB"))
		})
	}
}

// TestRunner_Stop_ClosesListeners verifies Stop tears the listeners down.
// Refs: FR-17.19
func TestRunner_Stop_ClosesListeners(t *testing.T) {
	r := testRunner(t, &dialRecorder{})
	ep, err := r.Start(context.Background(), Binding{
		SandboxID: "01SB", TaskID: "MGIT-11.7",
		GatewayIP: netip.MustParseAddr("127.0.0.1"),
		Policy:    allowlistPolicy("registry.npmjs.org"),
	})
	require.NoError(t, err)
	assert.True(t, r.Running("01SB"))

	require.NoError(t, r.Stop("01SB"))
	assert.False(t, r.Running("01SB"))

	// the proxy port is no longer accepting.
	_, err = net.DialTimeout("tcp", ep.ProxyAddr, 500*time.Millisecond)
	assert.Error(t, err, "the proxy listener is closed after Stop")
}

// TestRunner_DuplicateStart_Rejected guards against double-binding one
// sandbox. Refs: FR-17.1
func TestRunner_DuplicateStart_Rejected(t *testing.T) {
	r := testRunner(t, &dialRecorder{})
	b := Binding{SandboxID: "01SB", TaskID: "MGIT-11.7",
		GatewayIP: netip.MustParseAddr("127.0.0.1"), Policy: allowlistPolicy("registry.npmjs.org")}
	_, err := r.Start(context.Background(), b)
	require.NoError(t, err)
	defer func() { _ = r.Stop("01SB") }()
	_, err = r.Start(context.Background(), b)
	assert.Error(t, err, "a sandbox's egress cannot be started twice")
}

// TestNewRunner_Validates rejects missing dependencies (fail closed).
func TestNewRunner_Validates(t *testing.T) {
	_, err := NewRunner(RunnerConfig{})
	assert.Error(t, err)
}

func dnsQueryA(t *testing.T, name string) []byte {
	t.Helper()
	return buildQuery(t, name, dnsmessage.TypeA)
}
