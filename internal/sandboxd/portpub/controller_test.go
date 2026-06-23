package portpub

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

	"github.com/hyper-swe/mgit/internal/model"
)

func quietLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// fakePortDialer dials an arbitrary guest port on a sandbox, answering with a
// tiny echo "dev server", and records every (sandboxID, port) it dialed.
type fakePortDialer struct {
	mu      sync.Mutex
	dialed  []dial
	dialErr error
}

type dial struct {
	sandboxID string
	port      int
}

func (d *fakePortDialer) DialGuestPort(_ context.Context, sandboxID string, port int) (net.Conn, error) {
	d.mu.Lock()
	d.dialed = append(d.dialed, dial{sandboxID, port})
	d.mu.Unlock()
	if d.dialErr != nil {
		return nil, d.dialErr
	}
	host, guest := net.Pipe()
	go func() {
		defer func() { _ = guest.Close() }()
		buf := make([]byte, 4)
		if _, err := io.ReadFull(guest, buf); err != nil {
			return
		}
		_, _ = guest.Write(buf) // echo
	}()
	return host, nil
}

func (d *fakePortDialer) dials() []dial {
	d.mu.Lock()
	defer d.mu.Unlock()
	return append([]dial(nil), d.dialed...)
}

func testInfo(id string) model.SandboxInfo {
	return model.SandboxInfo{ID: id, TaskID: "MGIT-11.10.12"}
}

// freePort returns a currently-free, non-privileged host port by binding an
// ephemeral loopback listener and releasing it. There is a benign TOCTOU
// window before StartPublish rebinds it; acceptable for a unit test.
func freePort(t *testing.T) int {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	port := ln.Addr().(*net.TCPAddr).Port
	require.NoError(t, ln.Close())
	return port
}

// TestController_StartPublish_BindsLoopbackAndForwards verifies StartPublish
// binds a 127.0.0.1 listener per requested port and forwards into the guest
// over the dialer (host->guest, one way). Refs: SEC-09
func TestController_StartPublish_BindsLoopbackAndForwards(t *testing.T) {
	dialer := &fakePortDialer{}
	c, err := New(Config{Dialer: dialer, Logger: quietLogger()})
	require.NoError(t, err)
	defer c.StopPublish("sbx-1")

	err = c.StartPublish(context.Background(), testInfo("sbx-1"),
		[]model.PortPublish{{HostPort: freePort(t), GuestPort: 3000}})
	require.NoError(t, err)

	addrs := c.HostAddrs("sbx-1")
	require.Len(t, addrs, 1)
	tcp, ok := addrs[0].(*net.TCPAddr)
	require.True(t, ok)
	assert.True(t, tcp.IP.IsLoopback(), "the published port binds host loopback only, never 0.0.0.0")

	conn, err := net.Dial("tcp", addrs[0].String())
	require.NoError(t, err)
	defer func() { _ = conn.Close() }()
	_, err = conn.Write([]byte("ping"))
	require.NoError(t, err)
	buf := make([]byte, 4)
	_, err = io.ReadFull(conn, buf)
	require.NoError(t, err)
	assert.Equal(t, "ping", string(buf), "the host client reaches the guest dev server")

	assert.Eventually(t, func() bool {
		ds := dialer.dials()
		return len(ds) == 1 && ds[0] == dial{"sbx-1", 3000}
	}, time.Second, 10*time.Millisecond, "the host forwards into the guest's published port")
}

// TestController_StopPublish_NoResidue verifies teardown closes every host
// listener for a sandbox so nothing outlives it (FR-17.19). A second
// StopPublish is a safe no-op. Refs: SEC-09, FR-17.19
func TestController_StopPublish_NoResidue(t *testing.T) {
	c, err := New(Config{Dialer: &fakePortDialer{}, Logger: quietLogger()})
	require.NoError(t, err)

	require.NoError(t, c.StartPublish(context.Background(), testInfo("sbx-1"),
		[]model.PortPublish{{HostPort: freePort(t), GuestPort: 3000}, {HostPort: freePort(t), GuestPort: 5173}}))
	addr := c.HostAddrs("sbx-1")[0]

	c.StopPublish("sbx-1")
	assert.False(t, c.HasSandbox("sbx-1"), "no listener state remains for the torn-down sandbox")

	// The bound port is released: a dial to the closed listener fails.
	_, err = net.DialTimeout("tcp", addr.String(), 200*time.Millisecond)
	assert.Error(t, err, "the host listener is closed after teardown")

	c.StopPublish("sbx-1") // idempotent
}

// TestController_StartPublish_BindError_FailsClosed verifies a listen failure
// rolls back any already-opened listeners for the sandbox (no half-published
// residue) and returns an error so the caller fails the boot closed. Refs: SEC-09
func TestController_StartPublish_BindError_FailsClosed(t *testing.T) {
	c, err := New(Config{Dialer: &fakePortDialer{}, Logger: quietLogger()})
	require.NoError(t, err)
	defer c.StopPublish("sbx-1")

	// Occupy a fixed host port, then ask to publish it: the second bind fails.
	occupied, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	defer func() { _ = occupied.Close() }()
	busyPort := occupied.Addr().(*net.TCPAddr).Port

	err = c.StartPublish(context.Background(), testInfo("sbx-1"),
		[]model.PortPublish{{HostPort: freePort(t), GuestPort: 3000}, {HostPort: busyPort, GuestPort: 5173}})
	require.Error(t, err, "a bind failure fails closed")
	assert.False(t, c.HasSandbox("sbx-1"), "a failed StartPublish leaves no residual listeners")
}

// TestController_StartPublish_DuplicateSandbox verifies starting publish twice
// for the same live sandbox is rejected (the lifecycle publishes once on boot).
func TestController_StartPublish_DuplicateSandbox(t *testing.T) {
	c, err := New(Config{Dialer: &fakePortDialer{}, Logger: quietLogger()})
	require.NoError(t, err)
	defer c.StopPublish("sbx-1")
	require.NoError(t, c.StartPublish(context.Background(), testInfo("sbx-1"),
		[]model.PortPublish{{HostPort: freePort(t), GuestPort: 3000}}))
	err = c.StartPublish(context.Background(), testInfo("sbx-1"),
		[]model.PortPublish{{HostPort: freePort(t), GuestPort: 5173}})
	assert.Error(t, err, "a second StartPublish for a live sandbox is rejected")
}

// TestNew_Validates rejects missing dependencies (fail closed).
func TestNew_Validates(t *testing.T) {
	_, err := New(Config{})
	assert.Error(t, err)
	_, err = New(Config{Dialer: &fakePortDialer{}})
	assert.Error(t, err, "a logger is required")
}

// TestController_StartPublish_RejectsInvalidPort guards the model boundary even
// if a caller skipped validation (defense in depth). Refs: SEC-09
func TestController_StartPublish_RejectsInvalidPort(t *testing.T) {
	c, err := New(Config{Dialer: &fakePortDialer{}, Logger: quietLogger()})
	require.NoError(t, err)
	defer c.StopPublish("sbx-1")
	err = c.StartPublish(context.Background(), testInfo("sbx-1"),
		[]model.PortPublish{{HostPort: 80, GuestPort: 3000}})
	assert.Error(t, err, "a privileged host port is rejected before any bind")
	assert.False(t, c.HasSandbox("sbx-1"))
}
