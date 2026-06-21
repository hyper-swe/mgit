package egress

import (
	"context"
	"io"
	"log/slog"
	"net"
	"net/netip"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func quietLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// dialRecorder captures the dial target and returns one end of a pipe whose
// other end echoes everything written to it (a stand-in upstream).
type dialRecorder struct {
	mu       sync.Mutex
	dialed   []string
	dialErr  error
	upstream net.Conn
}

func (d *dialRecorder) dial(_ context.Context, ip netip.Addr, port int) (net.Conn, error) {
	d.mu.Lock()
	d.dialed = append(d.dialed, netip.AddrPortFrom(ip, uint16(port)).String())
	d.mu.Unlock()
	if d.dialErr != nil {
		return nil, d.dialErr
	}
	host, up := net.Pipe()
	d.upstream = up
	go func() { _, _ = io.Copy(up, up) }() // echo
	return host, nil
}

func (d *dialRecorder) targets() []string {
	d.mu.Lock()
	defer d.mu.Unlock()
	return append([]string(nil), d.dialed...)
}

func testProxy(t *testing.T, entries []string, lookup LookupFunc, dial DialFunc) *Proxy {
	t.Helper()
	aud := &fakeAuditor{}
	az := buildAuthorizer(t, entries, lookup, aud)
	p, err := NewProxy(ProxyConfig{Authorizer: az, Dial: dial, Logger: quietLogger(),
		HandshakeTimeout: time.Second})
	require.NoError(t, err)
	return p
}

// TestProxy_AllowedFlow_Splices verifies an allowed CONNECT splices bytes
// between guest and the host-dialed upstream, dialing the pinned IP.
// Refs: SEC-04, FR-17.8, MGIT-11.7.2
func TestProxy_AllowedFlow_Splices(t *testing.T) {
	rec := &dialRecorder{}
	p := testProxy(t, []string{"registry.npmjs.org"}, resolvesTo("140.82.112.3"), rec.dial)

	guest, host := net.Pipe()
	defer func() { _ = guest.Close() }()
	go p.handle(context.Background(), host)

	require.NoError(t, EncodeConnectRequest(guest, ConnectRequest{Protocol: "tcp", Host: "registry.npmjs.org", Port: 443}))
	allow, _, err := DecodeConnectReply(guest)
	require.NoError(t, err)
	require.True(t, allow, "the flow is allowed")

	_, err = guest.Write([]byte("ping"))
	require.NoError(t, err)
	buf := make([]byte, 4)
	_, err = io.ReadFull(guest, buf)
	require.NoError(t, err)
	assert.Equal(t, "ping", string(buf), "bytes splice to the echoing upstream and back")
	assert.Equal(t, []string{"140.82.112.3:443"}, rec.targets(), "dialed the host-resolved pinned IP")
}

// TestProxy_DeniedFlow_NoDial verifies a denied CONNECT returns the deny
// reply and never dials upstream. Refs: SEC-04, MGIT-11.7.2
func TestProxy_DeniedFlow_NoDial(t *testing.T) {
	rec := &dialRecorder{}
	p := testProxy(t, []string{"registry.npmjs.org"}, resolvesTo("140.82.112.3"), rec.dial)

	guest, host := net.Pipe()
	defer func() { _ = guest.Close() }()
	go p.handle(context.Background(), host)

	require.NoError(t, EncodeConnectRequest(guest, ConnectRequest{Protocol: "tcp", Host: "1.2.3.4", Port: 443}))
	allow, reason, err := DecodeConnectReply(guest)
	require.NoError(t, err)
	assert.False(t, allow, "non-allowlisted IP denied")
	assert.NotEmpty(t, reason, "deny carries a machine-readable reason for the guest")
	assert.Empty(t, rec.targets(), "a denied flow never dials upstream")
}

// TestProxy_OversizedHeader_Rejected verifies the request frame ceiling
// (a hostile guest cannot force unbounded host-side allocation).
// Refs: SEC-04 (land-path hardening class), MGIT-11.7.2
func TestProxy_OversizedHeader_Rejected(t *testing.T) {
	guest, host := net.Pipe()
	defer func() { _ = guest.Close() }()
	p := testProxy(t, []string{"registry.npmjs.org"}, nil, (&dialRecorder{}).dial)
	go p.handle(context.Background(), host)

	// Announce a frame larger than the ceiling.
	_, err := guest.Write([]byte{0xff, 0xff}) // 65535 > maxConnectRequestBytes
	require.NoError(t, err)
	_, _, err = DecodeConnectReply(guest)
	assert.Error(t, err, "the connection is closed without a reply on an oversized header")
}

// TestProxy_Serve_AcceptsAndStops verifies Serve accepts connections and
// returns when the listener closes. Refs: MGIT-11.7.2
func TestProxy_Serve_AcceptsAndStops(t *testing.T) {
	rec := &dialRecorder{}
	p := testProxy(t, []string{"registry.npmjs.org"}, resolvesTo("140.82.112.3"), rec.dial)

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)

	done := make(chan error, 1)
	go func() { done <- p.Serve(context.Background(), ln) }()

	c, err := net.Dial("tcp", ln.Addr().String())
	require.NoError(t, err)
	require.NoError(t, EncodeConnectRequest(c, ConnectRequest{Protocol: "tcp", Host: "registry.npmjs.org", Port: 443}))
	allow, _, err := DecodeConnectReply(c)
	require.NoError(t, err)
	assert.True(t, allow)
	_ = c.Close()

	require.NoError(t, ln.Close())
	select {
	case err := <-done:
		assert.NoError(t, err, "Serve returns cleanly when the listener closes")
	case <-time.After(2 * time.Second):
		t.Fatal("Serve did not return after listener close")
	}
}
