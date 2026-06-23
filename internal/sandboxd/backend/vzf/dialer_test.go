// vzf dialer tests cover the CGO-free heart of the macOS host->guest
// dialer: the live-VM registry and the sandbox-ID -> live-VM resolution
// with fail-closed semantics. The framework leg (VZVirtioSocketDevice.
// Connect on a real VZVirtualMachine) needs the virtualization
// entitlement + a guest image, so it is exercised only by the gated
// real-VM e2e (see dialer_e2e_darwin_test.go); here a fake guestConnector
// stands in for the live VM. Refs: FR-17.11, FR-17.16
package vzf

import (
	"context"
	"errors"
	"net"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/hyper-swe/mgit/internal/model"
	"github.com/hyper-swe/mgit/internal/sandboxd/backend/microvm"
)

// guestDialer must satisfy the transport seam the shared manager dials
// through; a drift here breaks microvm.Manager.Exec on macOS.
var _ microvm.GuestDialer = (*guestDialer)(nil)

// fakeConnector stands in for a live VM's framework connect: it records
// the dialed port and returns a preset conn or error.
type fakeConnector struct {
	conn    net.Conn
	err     error
	gotPort uint32
	calls   int
}

func (f *fakeConnector) connectGuest(port uint32) (net.Conn, error) {
	f.calls++
	f.gotPort = port
	if f.err != nil {
		return nil, f.err
	}
	return f.conn, nil
}

// TestGuestDialer_DialGuest_RegisteredVM_ConnectsOnExecPort verifies the
// dialer resolves a registered sandbox to its live VM and connects on the
// shared guest exec port, returning a usable stream. Refs: FR-17.11
func TestGuestDialer_DialGuest_RegisteredVM_ConnectsOnExecPort(t *testing.T) {
	host, guest := net.Pipe()
	defer func() { _ = host.Close() }()
	defer func() { _ = guest.Close() }()

	fake := &fakeConnector{conn: host}
	reg := newLiveVMs()
	reg.put("sb-running", fake)

	conn, err := newGuestExecDialer(reg).DialGuest(context.Background(), "sb-running")
	require.NoError(t, err)
	require.NotNil(t, conn)
	assert.Equal(t, microvm.GuestExecPort, fake.gotPort, "exec channel dials the single-sourced exec port (1024)")
	assert.Equal(t, 1, fake.calls)

	// The returned conn is the live channel: a guest write reaches the host.
	go func() { _, _ = guest.Write([]byte("hi")) }()
	buf := make([]byte, 2)
	n, err := conn.Read(buf)
	require.NoError(t, err)
	assert.Equal(t, "hi", string(buf[:n]))
}

// TestGuestDialer_DialGuest_RegisteredVM_ConnectsOnLandPort verifies the
// land dialer resolves a registered sandbox to its live VM and connects on
// the shared guest LAND port (distinct from the exec port), so the host
// pulls the task pool over the dedicated land channel. It shares the connect
// mechanism with exec; only the port differs. Refs: FR-17.5, FR-17.16
func TestGuestDialer_DialGuest_RegisteredVM_ConnectsOnLandPort(t *testing.T) {
	host, guest := net.Pipe()
	defer func() { _ = host.Close() }()
	defer func() { _ = guest.Close() }()

	fake := &fakeConnector{conn: host}
	reg := newLiveVMs()
	reg.put("sb-running", fake)

	conn, err := newGuestLandDialer(reg).DialGuest(context.Background(), "sb-running")
	require.NoError(t, err)
	require.NotNil(t, conn)
	assert.Equal(t, microvm.GuestLandPort, fake.gotPort, "land channel dials the single-sourced land port (1025)")
	assert.NotEqual(t, microvm.GuestExecPort, fake.gotPort, "land must not collide with the exec port")
	assert.Equal(t, 1, fake.calls)

	// The returned conn is the live channel: a guest write reaches the host.
	go func() { _, _ = guest.Write([]byte("ld")) }()
	buf := make([]byte, 2)
	n, err := conn.Read(buf)
	require.NoError(t, err)
	assert.Equal(t, "ld", string(buf[:n]))
}

// TestGuestLandDialer_DialGuest_UnknownSandbox_FailsClosed verifies the land
// dialer fails closed for a sandbox with no live VM rather than dialing
// anything. Refs: FR-17.5, FR-17.16
func TestGuestLandDialer_DialGuest_UnknownSandbox_FailsClosed(t *testing.T) {
	conn, err := newGuestLandDialer(newLiveVMs()).DialGuest(context.Background(), "ghost")
	assert.Nil(t, conn)
	require.Error(t, err)
	assert.ErrorIs(t, err, model.ErrSandboxBackendUnavailable)
}

// TestGuestLandDialer_DialGuest_AfterRemove_FailsClosed verifies a torn-down
// sandbox is no longer reachable on the land channel either, so a stale ID
// cannot pull a successor's pool. Refs: FR-17.5, FR-17.16, SEC-10
func TestGuestLandDialer_DialGuest_AfterRemove_FailsClosed(t *testing.T) {
	reg := newLiveVMs()
	reg.put("sb-gone", &fakeConnector{conn: nil})
	reg.remove("sb-gone")

	conn, err := newGuestLandDialer(reg).DialGuest(context.Background(), "sb-gone")
	assert.Nil(t, conn)
	assert.ErrorIs(t, err, model.ErrSandboxBackendUnavailable)
}

// TestGuestDialer_DialGuest_UnknownSandbox_FailsClosed verifies a sandbox
// with no live VM fails closed rather than dialing anything. Refs: FR-17.16
func TestGuestDialer_DialGuest_UnknownSandbox_FailsClosed(t *testing.T) {
	conn, err := newGuestExecDialer(newLiveVMs()).DialGuest(context.Background(), "ghost")
	assert.Nil(t, conn)
	require.Error(t, err)
	assert.ErrorIs(t, err, model.ErrSandboxBackendUnavailable)
}

// TestGuestDialer_DialGuest_ConnectorError_FailsClosed verifies a framework
// connect failure (e.g. the VM has no socket device) propagates as an
// error with no leaked conn. Refs: FR-17.16
func TestGuestDialer_DialGuest_ConnectorError_FailsClosed(t *testing.T) {
	sentinel := errors.New("vsock device absent")
	reg := newLiveVMs()
	reg.put("sb-no-socket", &fakeConnector{err: sentinel})

	conn, err := newGuestExecDialer(reg).DialGuest(context.Background(), "sb-no-socket")
	assert.Nil(t, conn)
	require.ErrorIs(t, err, sentinel)
}

// TestGuestDialer_DialGuest_AfterRemove_FailsClosed verifies a sandbox torn
// down (deregistered) is no longer dialable, so a stale ID cannot reach a
// successor channel. Refs: FR-17.16, SEC-10
func TestGuestDialer_DialGuest_AfterRemove_FailsClosed(t *testing.T) {
	reg := newLiveVMs()
	reg.put("sb-gone", &fakeConnector{conn: nil})
	reg.remove("sb-gone")

	conn, err := newGuestExecDialer(reg).DialGuest(context.Background(), "sb-gone")
	assert.Nil(t, conn)
	assert.ErrorIs(t, err, model.ErrSandboxBackendUnavailable)
}

// TestLiveVMs_PutGetRemove covers the registry's lookup lifecycle.
func TestLiveVMs_PutGetRemove(t *testing.T) {
	reg := newLiveVMs()
	_, ok := reg.get("absent")
	assert.False(t, ok, "empty registry resolves nothing")

	c := &fakeConnector{}
	reg.put("sb-1", c)
	got, ok := reg.get("sb-1")
	require.True(t, ok)
	assert.Same(t, c, got)

	reg.remove("sb-1")
	_, ok = reg.get("sb-1")
	assert.False(t, ok, "removed sandbox resolves nothing")
}

// TestLiveVMs_ConcurrentAccess exercises the registry under the race
// detector: concurrent put/get/remove must be data-race-free, since the
// manager launches and tears down sandboxes from many goroutines.
func TestLiveVMs_ConcurrentAccess(t *testing.T) {
	reg := newLiveVMs()
	var wg sync.WaitGroup
	for i := 0; i < 16; i++ {
		wg.Add(1)
		go func(id string) {
			defer wg.Done()
			reg.put(id, &fakeConnector{})
			_, _ = reg.get(id)
			reg.remove(id)
		}(string(rune('a' + i)))
	}
	wg.Wait()
}
