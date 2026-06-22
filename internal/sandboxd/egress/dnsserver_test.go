package egress

import (
	"context"
	"errors"
	"net"
	"net/netip"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"golang.org/x/net/dns/dnsmessage"
)

func buildQuery(t *testing.T, name string, typ dnsmessage.Type) []byte {
	t.Helper()
	b := dnsmessage.NewBuilder(nil, dnsmessage.Header{ID: 0x1234, RecursionDesired: true})
	require.NoError(t, b.StartQuestions())
	require.NoError(t, b.Question(dnsmessage.Question{
		Name: dnsmessage.MustNewName(name + "."), Type: typ, Class: dnsmessage.ClassINET,
	}))
	msg, err := b.Finish()
	require.NoError(t, err)
	return msg
}

func parseResp(t *testing.T, raw []byte) (dnsmessage.Header, []dnsmessage.Resource) {
	t.Helper()
	var p dnsmessage.Parser
	hdr, err := p.Start(raw)
	require.NoError(t, err)
	require.NoError(t, p.SkipAllQuestions())
	answers, err := p.AllAnswers()
	if errors.Is(err, dnsmessage.ErrSectionDone) {
		answers = nil
	} else {
		require.NoError(t, err)
	}
	return hdr, answers
}

func dnsTestServer(t *testing.T, lookup LookupFunc) *DNSServer {
	t.Helper()
	r := testResolver(t, lookup, &fakeAuditor{}, frozenClock())
	srv, err := NewDNSServer(r, quietLogger())
	require.NoError(t, err)
	return srv
}

// TestDNSServer_AllowlistedNameResolves verifies an A query for an
// allowlisted name returns the host-resolved address and pins it. Refs: SEC-07
func TestDNSServer_AllowlistedNameResolves(t *testing.T) {
	srv := dnsTestServer(t, resolvesTo("140.82.112.3"))
	resp := srv.handleQuery(context.Background(), buildQuery(t, "registry.npmjs.org", dnsmessage.TypeA))
	require.NotNil(t, resp)

	hdr, answers := parseResp(t, resp)
	assert.True(t, hdr.Response)
	assert.Equal(t, dnsmessage.RCodeSuccess, hdr.RCode)
	require.Len(t, answers, 1)
	a, ok := answers[0].Body.(*dnsmessage.AResource)
	require.True(t, ok)
	assert.Equal(t, [4]byte{140, 82, 112, 3}, a.A)

	// the answer is pinned so the proxy admits the subsequent connection.
	assert.True(t, srv.resolver.IsPinned(netip.MustParseAddr("140.82.112.3")))
}

// TestDNSServer_NonAllowlistedRefused verifies a query for a non-allowlisted
// name is refused (no answer, no upstream leak). Refs: SEC-07
func TestDNSServer_NonAllowlistedRefused(t *testing.T) {
	var consulted bool
	srv := dnsTestServer(t, func(context.Context, string) ([]netip.Addr, error) {
		consulted = true
		return []netip.Addr{netip.MustParseAddr("1.2.3.4")}, nil
	})
	resp := srv.handleQuery(context.Background(), buildQuery(t, "evil.example.com", dnsmessage.TypeA))
	hdr, answers := parseResp(t, resp)
	assert.Equal(t, dnsmessage.RCodeRefused, hdr.RCode, "non-allowlisted name is refused")
	assert.Empty(t, answers)
	assert.False(t, consulted, "the upstream resolver is never consulted for a non-allowlisted name")
}

// TestDNSServer_AAAA verifies AAAA queries return only IPv6 answers.
func TestDNSServer_AAAA(t *testing.T) {
	srv := dnsTestServer(t, func(context.Context, string) ([]netip.Addr, error) {
		return []netip.Addr{netip.MustParseAddr("140.82.112.3"), netip.MustParseAddr("2606:50c0::153")}, nil
	})
	hdr, answers := parseResp(t, srv.handleQuery(context.Background(), buildQuery(t, "registry.npmjs.org", dnsmessage.TypeAAAA)))
	assert.Equal(t, dnsmessage.RCodeSuccess, hdr.RCode)
	require.Len(t, answers, 1, "only the AAAA record for an AAAA query")
	_, ok := answers[0].Body.(*dnsmessage.AAAAResource)
	assert.True(t, ok)
}

// TestDNSServer_MalformedQuery verifies a malformed packet is handled
// gracefully (a FormErr response or a drop), never a panic. Refs: SEC-04
func TestDNSServer_MalformedQuery(t *testing.T) {
	srv := dnsTestServer(t, resolvesTo("140.82.112.3"))
	assert.NotPanics(t, func() {
		_ = srv.handleQuery(context.Background(), []byte{0x00, 0x01, 0x02})
	})
}

// TestDNSServer_ServeUDP_RoundTrip drives a real query over a loopback UDP
// socket through ServeUDP. Refs: SEC-07
func TestDNSServer_ServeUDP_RoundTrip(t *testing.T) {
	srv := dnsTestServer(t, resolvesTo("140.82.112.3"))

	pc, err := net.ListenPacket("udp", "127.0.0.1:0")
	require.NoError(t, err)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { _ = srv.ServeUDP(ctx, pc) }()

	client, err := net.Dial("udp", pc.LocalAddr().String())
	require.NoError(t, err)
	defer func() { _ = client.Close() }()
	_, err = client.Write(buildQuery(t, "registry.npmjs.org", dnsmessage.TypeA))
	require.NoError(t, err)

	_ = client.SetReadDeadline(time.Now().Add(2 * time.Second))
	buf := make([]byte, 512)
	n, err := client.Read(buf)
	require.NoError(t, err)
	hdr, answers := parseResp(t, buf[:n])
	assert.Equal(t, dnsmessage.RCodeSuccess, hdr.RCode)
	require.Len(t, answers, 1)
}

// TestNewDNSServer_Validates rejects missing dependencies (fail closed).
func TestNewDNSServer_Validates(t *testing.T) {
	_, err := NewDNSServer(nil, quietLogger())
	assert.Error(t, err)
	r := testResolver(t, resolvesTo("140.82.112.3"), &fakeAuditor{}, frozenClock())
	_, err = NewDNSServer(r, nil)
	assert.Error(t, err)
}
