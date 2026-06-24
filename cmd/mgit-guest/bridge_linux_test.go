//go:build linux

package main

import (
	"context"
	"io"
	"log/slog"
	"net"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// fakeVsockListener is an in-memory net.Listener: the test "host" hands it
// connections via inject; Accept hands them to the bridge. It models the
// AF_VSOCK listener without a real vsock device (CGO-free). Refs: SEC-09
type fakeVsockListener struct {
	conns  chan net.Conn
	closed chan struct{}
	once   sync.Once
}

func newFakeVsockListener() *fakeVsockListener {
	return &fakeVsockListener{conns: make(chan net.Conn), closed: make(chan struct{})}
}

func (l *fakeVsockListener) Accept() (net.Conn, error) {
	select {
	case c := <-l.conns:
		return c, nil
	case <-l.closed:
		return nil, net.ErrClosed
	}
}

func (l *fakeVsockListener) Close() error {
	l.once.Do(func() { close(l.closed) })
	return nil
}

func (l *fakeVsockListener) Addr() net.Addr { return dummyAddr{} }

type dummyAddr struct{}

func (dummyAddr) Network() string { return "vsock" }
func (dummyAddr) String() string  { return "vsock" }

// quietLogger discards bridge logs in tests.
func quietLogger() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

// TestServePublishBridges_HostConnectReachesLoopbackTCP proves a host-initiated
// vsock connect is forwarded to the guest's loopback TCP service and bytes
// flow both ways — the publish direction (SEC-09). The "dev server" echoes a
// fixed body; the "host" reads it back through the bridge.
func TestServePublishBridges_HostConnectReachesLoopbackTCP(t *testing.T) {
	const guestPort = 3000
	const body = "hello-from-guest"

	ln := newFakeVsockListener()
	listen := func(uint32) (net.Listener, error) { return ln, nil }

	// dial models the guest's loopback dev server: each dial returns one end of
	// an in-memory pipe whose other end runs a tiny echo-a-body server. This is
	// the ONLY thing the bridge dials — never the host (SEC-09 one-way).
	dial := func(_ context.Context, port int) (net.Conn, error) {
		require.Equal(t, guestPort, port, "the bridge dials the SAME port it listened on, on loopback")
		serverSide, devSide := net.Pipe()
		go func() {
			defer func() { _ = serverSide.Close() }()
			buf := make([]byte, 64)
			_, _ = serverSide.Read(buf) // consume the host's request
			_, _ = io.WriteString(serverSide, body)
		}()
		return devSide, nil
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan struct{})
	go func() {
		servePublishBridges(ctx, []int{guestPort}, listen, dial, quietLogger())
		close(done)
	}()

	// The "host" side: hand the bridge a connection and exchange bytes.
	hostSide, bridgeSide := net.Pipe()
	ln.conns <- bridgeSide
	_ = hostSide.SetDeadline(time.Now().Add(2 * time.Second))
	_, err := io.WriteString(hostSide, "GET /\r\n")
	require.NoError(t, err)
	got, err := io.ReadAll(hostSide)
	require.NoError(t, err)
	assert.Equal(t, body, string(got), "the host reaches the guest dev server through the bridge")

	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("bridges did not stop on context cancel (goroutine leak)")
	}
}

// TestServePublishBridges_NoPorts_NoOp verifies an empty port list starts no
// bridges and returns immediately.
func TestServePublishBridges_NoPorts_NoOp(t *testing.T) {
	done := make(chan struct{})
	go func() {
		servePublishBridges(context.Background(), nil, nil, nil, quietLogger())
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("servePublishBridges with no ports must return immediately")
	}
}

// TestServePublishBridges_ListenError_SkipsPort verifies a port whose vsock
// listen fails is skipped (best-effort) without wedging others or panicking.
func TestServePublishBridges_ListenError_SkipsPort(t *testing.T) {
	listen := func(uint32) (net.Listener, error) { return nil, net.ErrClosed }
	done := make(chan struct{})
	go func() {
		servePublishBridges(context.Background(), []int{3000}, listen, nil, quietLogger())
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("a port that fails to listen must be skipped, not block")
	}
}

// TestForwardToLoopback_DialFails_DropsConnection verifies that when the
// loopback dev server is not up, the bridge drops the host connection cleanly
// (no path opened, no panic). Refs: SEC-09
func TestForwardToLoopback_DialFails_DropsConnection(t *testing.T) {
	hostSide, bridgeSide := net.Pipe()
	defer func() { _ = hostSide.Close() }()
	dial := func(context.Context, int) (net.Conn, error) { return nil, net.ErrClosed }

	done := make(chan struct{})
	go func() {
		forwardToLoopback(context.Background(), 3000, bridgeSide, dial, quietLogger())
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("forwardToLoopback must drop the connection when the dial fails")
	}
	// The bridge closed its side: a read returns EOF/closed.
	_ = hostSide.SetReadDeadline(time.Now().Add(time.Second))
	_, err := hostSide.Read(make([]byte, 1))
	assert.Error(t, err, "the bridge closes the host connection when loopback dial fails")
}
