package egress

import (
	"context"
	"io"
	"net"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// fakeGuestPortDialer dials a guest-published port. It returns one end of a
// pipe whose other end runs a tiny "dev server" that replies to every
// request, and records every port it was asked to dial.
type fakeGuestPortDialer struct {
	mu      sync.Mutex
	dialed  []int
	dialErr error
	reply   string
}

func (d *fakeGuestPortDialer) DialGuestPort(_ context.Context, port int) (net.Conn, error) {
	d.mu.Lock()
	d.dialed = append(d.dialed, port)
	d.mu.Unlock()
	if d.dialErr != nil {
		return nil, d.dialErr
	}
	host, guest := net.Pipe()
	go func() {
		defer func() { _ = guest.Close() }()
		buf := make([]byte, 16)
		if _, err := guest.Read(buf); err != nil {
			return
		}
		_, _ = io.WriteString(guest, d.reply)
	}()
	return host, nil
}

func (d *fakeGuestPortDialer) dials() []int {
	d.mu.Lock()
	defer d.mu.Unlock()
	return append([]int(nil), d.dialed...)
}

// TestPortPublish_GuestServiceReachableOnHost verifies a guest-published
// port (e.g. a dev server) is reachable at host loopback, forwarded into
// the guest over the injected dialer. Refs: SEC-09, MGIT-11.7.4
func TestPortPublish_GuestServiceReachableOnHost(t *testing.T) {
	dialer := &fakeGuestPortDialer{reply: "HTTP/1.1 200 OK"}
	pub, err := NewPublisher(PublisherConfig{Dialer: dialer, Logger: quietLogger()})
	require.NoError(t, err)

	ln, err := pub.Publish(context.Background(), 3000)
	require.NoError(t, err)
	defer func() { _ = ln.Close() }()

	host, ok := ln.Addr().(*net.TCPAddr)
	require.True(t, ok)
	assert.True(t, host.IP.IsLoopback(), "the published port binds host loopback, not 0.0.0.0")

	c, err := net.Dial("tcp", ln.Addr().String())
	require.NoError(t, err)
	defer func() { _ = c.Close() }()
	_, err = io.WriteString(c, "GET / HTTP/1.1\r\n")
	require.NoError(t, err)
	buf := make([]byte, len("HTTP/1.1 200 OK"))
	_, err = io.ReadFull(c, buf)
	require.NoError(t, err)
	assert.Equal(t, "HTTP/1.1 200 OK", string(buf), "the host client reaches the guest dev server")
	assert.Equal(t, []int{3000}, dialer.dials(), "the host forwards into the guest's published port")
}

// TestPortPublish_HostLoopbackUnreachableFromGuest verifies the guest
// cannot reach host loopback services (e.g. a host DB on 127.0.0.1:5432):
// the guest's only egress is the proxy, which unconditionally denies
// loopback. Refs: SEC-09, SEC-04, MGIT-11.7.4
func TestPortPublish_HostLoopbackUnreachableFromGuest(t *testing.T) {
	az := buildAuthorizer(t, []string{"registry.npmjs.org"}, resolvesTo("140.82.112.3"), &fakeAuditor{})
	d, err := az.Authorize(context.Background(), Flow{Protocol: "tcp", Host: "127.0.0.1", Port: 5432})
	assert.ErrorIs(t, err, ErrEgressDenied, "a guest connection to a host loopback service is denied")
	assert.False(t, d.Allow)
}

// TestPortPublish_OneWayOnly verifies forwarding is strictly one-way: the
// host listener initiates the guest dial on each accept, and there is no
// path by which the guest opens a connection toward the host. Closing the
// host side tears the forwarded connection down. Refs: SEC-09, MGIT-11.7.4
func TestPortPublish_OneWayOnly(t *testing.T) {
	dialer := &fakeGuestPortDialer{reply: "ok"}
	pub, err := NewPublisher(PublisherConfig{Dialer: dialer, Logger: quietLogger()})
	require.NoError(t, err)

	ln, err := pub.Publish(context.Background(), 8080)
	require.NoError(t, err)
	defer func() { _ = ln.Close() }()

	// No guest dial happens until a host client connects (host-initiated only).
	assert.Empty(t, dialer.dials(), "no guest dial before a host connection")

	c, err := net.Dial("tcp", ln.Addr().String())
	require.NoError(t, err)
	_, err = io.WriteString(c, "ping")
	require.NoError(t, err)
	buf := make([]byte, 2)
	_, err = io.ReadFull(c, buf)
	require.NoError(t, err)
	assert.Equal(t, "ok", string(buf))
	require.NoError(t, c.Close())

	assert.Eventually(t, func() bool { return len(dialer.dials()) == 1 }, time.Second, 10*time.Millisecond,
		"exactly one guest dial, initiated by the host accept")
}

// TestNewPublisher_Validates rejects missing dependencies (fail closed).
func TestNewPublisher_Validates(t *testing.T) {
	_, err := NewPublisher(PublisherConfig{})
	assert.Error(t, err)
	_, err = NewPublisher(PublisherConfig{Dialer: &fakeGuestPortDialer{}})
	assert.Error(t, err, "a logger is required")
}

// TestPublish_RejectsBadPort guards the port range.
func TestPublish_RejectsBadPort(t *testing.T) {
	pub, err := NewPublisher(PublisherConfig{Dialer: &fakeGuestPortDialer{}, Logger: quietLogger()})
	require.NoError(t, err)
	_, err = pub.Publish(context.Background(), 0)
	assert.Error(t, err)
	_, err = pub.Publish(context.Background(), 70000)
	assert.Error(t, err)
}
