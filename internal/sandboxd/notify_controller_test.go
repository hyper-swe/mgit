package sandboxd

import (
	"fmt"
	"net"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// tempSockListen is a ListenFunc that binds a real unix socket under a test
// dir, standing in for the firecracker reverse-vsock per-VM listener.
func tempSockListen(t *testing.T) (ListenFunc, func(sandboxID string) string) {
	t.Helper()
	skipUnsupportedHostIPC(t)
	dir, err := os.MkdirTemp("", "ntfy")
	require.NoError(t, err)
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	pathFor := func(sandboxID string) string { return filepath.Join(dir, sandboxID+".sock") }
	return UnixListen, pathFor
}

func newTestController(t *testing.T, binder *PeerBinder, lander NotifyLander, listen ListenFunc) *NotifyController {
	t.Helper()
	ctrl, err := NewNotifyController(binder, listen, notifyTestLogger())
	require.NoError(t, err)
	ctrl.SetLander(lander)
	return ctrl
}

// TestNotifyController_Register_StartsTriggeringListener proves Register opens a
// per-VM listener whose authorized notification runs the verified land for the
// host-bound task recorded at registration. Refs: MGIT-11.10.11
func TestNotifyController_Register_StartsTriggeringListener(t *testing.T) {
	binder := NewPeerBinder(notifyTestLogger())
	binder.Bind("sbx-A", "peer-A")
	lander := &fakeNotifyLander{}
	listen, pathFor := tempSockListen(t)
	ctrl := newTestController(t, binder, lander, listen)

	sock := pathFor("sbx-A")
	require.NoError(t, ctrl.Register("sbx-A", "MGIT-1.1", "peer-A", sock))
	t.Cleanup(func() { ctrl.Deregister("sbx-A") })

	dialNotify(t, sock)
	calls, tasks := waitForLands(lander, 1)
	require.Equal(t, 1, calls)
	assert.Equal(t, []string{"MGIT-1.1"}, tasks)
}

// TestNotifyController_TaskFor_HostAnchored proves TaskFor reflects only the
// host's own per-VM binding and goes empty after Deregister (no recycled
// binding survives teardown). Refs: SEC-05, SEC-10
func TestNotifyController_TaskFor_HostAnchored(t *testing.T) {
	binder := NewPeerBinder(notifyTestLogger())
	binder.Bind("sbx-A", "peer-A")
	listen, pathFor := tempSockListen(t)
	ctrl := newTestController(t, binder, &fakeNotifyLander{}, listen)

	_, ok := ctrl.TaskFor("sbx-A")
	assert.False(t, ok, "unknown sandbox has no host-bound task")

	require.NoError(t, ctrl.Register("sbx-A", "MGIT-1.1", "peer-A", pathFor("sbx-A")))
	task, ok := ctrl.TaskFor("sbx-A")
	require.True(t, ok)
	assert.Equal(t, "MGIT-1.1", task)

	ctrl.Deregister("sbx-A")
	_, ok = ctrl.TaskFor("sbx-A")
	assert.False(t, ok, "teardown drops the binding")
}

// TestNotifyController_Deregister_StopsListener proves Deregister closes the
// per-VM listener so the trigger no longer fires after teardown. A torn-down
// sandbox can no longer auto-land. Refs: MGIT-11.10.11, FR-17.19
func TestNotifyController_Deregister_StopsListener(t *testing.T) {
	binder := NewPeerBinder(notifyTestLogger())
	binder.Bind("sbx-A", "peer-A")
	lander := &fakeNotifyLander{}
	listen, pathFor := tempSockListen(t)
	ctrl := newTestController(t, binder, lander, listen)

	sock := pathFor("sbx-A")
	require.NoError(t, ctrl.Register("sbx-A", "MGIT-1.1", "peer-A", sock))
	ctrl.Deregister("sbx-A")

	// After teardown the per-VM listener is closed (close is async via the
	// serve goroutine, so poll): a dial to the torn-down socket fails, and
	// nothing lands.
	require.Eventually(t, func() bool {
		c, err := net.Dial("unix", sock)
		if err == nil {
			_ = c.Close()
			return false
		}
		return true
	}, 2*time.Second, 10*time.Millisecond, "the per-VM notify socket is closed at teardown")
	calls, _ := waitForLands(lander, 1)
	assert.Equal(t, 0, calls)
}

// TestNotifyController_TwoGuests_Isolated is the controller-level F-E
// regression: two registered guests on distinct per-VM sockets and distinct
// host-observed identities. Guest A's notify lands ONLY A's task; B is never
// triggered. Refs: MGIT-11.10.11, SEC-10
func TestNotifyController_TwoGuests_Isolated(t *testing.T) {
	binder := NewPeerBinder(notifyTestLogger())
	binder.Bind("sbx-A", "peer-A")
	binder.Bind("sbx-B", "peer-B")
	lander := &fakeNotifyLander{}
	listen, pathFor := tempSockListen(t)
	ctrl := newTestController(t, binder, lander, listen)

	require.NoError(t, ctrl.Register("sbx-A", "MGIT-A.1", "peer-A", pathFor("sbx-A")))
	require.NoError(t, ctrl.Register("sbx-B", "MGIT-B.1", "peer-B", pathFor("sbx-B")))
	t.Cleanup(func() { ctrl.Deregister("sbx-A"); ctrl.Deregister("sbx-B") })

	dialNotify(t, pathFor("sbx-A"))
	calls, tasks := waitForLands(lander, 1)
	require.Equal(t, 1, calls)
	assert.Equal(t, []string{"MGIT-A.1"}, tasks, "guest A's notify cannot land guest B's task")
}

// TestNewNotifyController_NilDeps_Rejected proves the constructor fails closed
// on a nil dependency.
func TestNewNotifyController_NilDeps_Rejected(t *testing.T) {
	binder := NewPeerBinder(notifyTestLogger())
	listen := func(string) (net.Listener, error) { return nil, fmt.Errorf("unused") }

	_, err := NewNotifyController(nil, listen, notifyTestLogger())
	require.Error(t, err)
	_, err = NewNotifyController(binder, nil, notifyTestLogger())
	require.Error(t, err)
	_, err = NewNotifyController(binder, listen, nil)
	require.Error(t, err)
}

// TestNotifyController_NoLanderWired_FailsClosed proves an authorized trigger
// before SetLander runs no land — the trigger never imports objects itself
// (SEC-01). Refs: MGIT-11.10.11, SEC-01
func TestNotifyController_NoLanderWired_FailsClosed(t *testing.T) {
	binder := NewPeerBinder(notifyTestLogger())
	binder.Bind("sbx-A", "peer-A")
	listen, pathFor := tempSockListen(t)
	ctrl, err := NewNotifyController(binder, listen, notifyTestLogger())
	require.NoError(t, err)
	// No SetLander.
	require.NoError(t, ctrl.Register("sbx-A", "MGIT-1.1", "peer-A", pathFor("sbx-A")))
	t.Cleanup(func() { ctrl.Deregister("sbx-A") })

	_, _, lerr := ctrl.Land(nil, "MGIT-1.1") //nolint:staticcheck // exercising the nil-lander guard directly
	require.Error(t, lerr, "no verified land path is wired yet")
}

// TestNotifyController_Register_ListenError_Reported proves a listen failure is
// surfaced (the auto-land trigger is best-effort; the host-initiated land path
// is unaffected).
func TestNotifyController_Register_ListenError_Reported(t *testing.T) {
	binder := NewPeerBinder(notifyTestLogger())
	failListen := func(string) (net.Listener, error) { return nil, fmt.Errorf("bind refused") }
	ctrl := newTestController(t, binder, &fakeNotifyLander{}, failListen)
	err := ctrl.Register("sbx-A", "MGIT-1.1", "peer-A", "/nonexistent/x.sock")
	require.Error(t, err)
}
