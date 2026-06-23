package sandboxd

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// fakeNotifyLander records each triggered land and the task it ran for, so a
// test can assert the trigger reached the verified land path (or never did,
// when authorization fails closed).
type fakeNotifyLander struct {
	mu    sync.Mutex
	calls int
	tasks []string
	err   error
}

func (l *fakeNotifyLander) Land(_ context.Context, taskID string) (int, string, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.calls++
	l.tasks = append(l.tasks, taskID)
	if l.err != nil {
		return 0, "", l.err
	}
	return 1, "task/" + taskID, nil
}

func (l *fakeNotifyLander) snapshot() (int, []string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.calls, append([]string(nil), l.tasks...)
}

// staticResolver is a host-anchored sandbox->task map for the NotifyServer
// tests (the controller is exercised separately). A missing sandbox returns
// not-found so the fail-closed path is covered.
type staticResolver map[string]string

func (r staticResolver) TaskFor(sandboxID string) (string, bool) {
	t, ok := r[sandboxID]
	return t, ok
}

func notifyTestLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// serveOnePair starts the NotifyServer on a fresh unix listener bound for
// (sandboxID, peerID) and returns the socket path; the listener stops when the
// returned cancel is called or the test ends.
func serveOnePair(t *testing.T, srv *NotifyServer, sandboxID, peerID string) string {
	t.Helper()
	skipUnsupportedHostIPC(t)
	sock := shortSocketPath(t)
	ln, err := UnixListen(sock)
	require.NoError(t, err)
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	go func() { _ = srv.Serve(ctx, sandboxID, peerID, ln) }()
	return sock
}

// dialNotify sends one guest->host notification (carrying no data) to the host
// notify socket, then closes — exactly what a guest emits.
func dialNotify(t *testing.T, sock string) {
	t.Helper()
	conn, err := net.DialTimeout("unix", sock, time.Second)
	require.NoError(t, err)
	_ = conn.Close()
}

// waitForLands blocks until the lander has recorded want calls or the deadline
// passes, then returns the observed count and tasks.
func waitForLands(l *fakeNotifyLander, want int) (int, []string) {
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if n, _ := l.snapshot(); n >= want {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	return l.snapshot()
}

// TestNotifyServer_AuthorizedPeer_TriggersVerifiedLand proves an authorized
// guest->host notification runs the verified host-initiated land for the
// sandbox's HOST-bound task — the notify is a trigger, the land does the work.
// Refs: MGIT-11.10.11, SEC-01
func TestNotifyServer_AuthorizedPeer_TriggersVerifiedLand(t *testing.T) {
	binder := NewPeerBinder(notifyTestLogger())
	binder.Bind("sbx-A", "peer-A")
	lander := &fakeNotifyLander{}
	srv, err := NewNotifyServer(binder, staticResolver{"sbx-A": "MGIT-1.1"}, lander, notifyTestLogger())
	require.NoError(t, err)

	sock := serveOnePair(t, srv, "sbx-A", "peer-A")
	dialNotify(t, sock)

	calls, tasks := waitForLands(lander, 1)
	require.Equal(t, 1, calls, "an authorized notify triggers exactly one land")
	assert.Equal(t, []string{"MGIT-1.1"}, tasks, "land runs for the HOST-bound task, never guest text")
}

// TestNotifyServer_UnboundSandbox_FailsClosed proves a notification for a
// sandbox with no live binding (never launched or torn down) is refused and
// NEVER reaches the land path. Refs: MGIT-11.10.11, SEC-10
func TestNotifyServer_UnboundSandbox_FailsClosed(t *testing.T) {
	binder := NewPeerBinder(notifyTestLogger()) // no bindings
	lander := &fakeNotifyLander{}
	srv, err := NewNotifyServer(binder, staticResolver{"sbx-A": "MGIT-1.1"}, lander, notifyTestLogger())
	require.NoError(t, err)

	sock := serveOnePair(t, srv, "sbx-A", "peer-A")
	dialNotify(t, sock)

	// Give the (rejected) handler time to run, then assert nothing landed.
	calls, _ := waitForLands(lander, 1)
	assert.Equal(t, 0, calls, "an unbound sandbox fails closed: no land")
}

// TestNotifyServer_MismatchedPeer_FailsClosed proves a notification whose
// host-observed socket identity does not match the sandbox's launch binding is
// refused — the trigger cannot fire for a peer it is not bound as.
// Refs: MGIT-11.10.11, SEC-10
func TestNotifyServer_MismatchedPeer_FailsClosed(t *testing.T) {
	binder := NewPeerBinder(notifyTestLogger())
	binder.Bind("sbx-A", "peer-A") // bound to peer-A
	lander := &fakeNotifyLander{}
	srv, err := NewNotifyServer(binder, staticResolver{"sbx-A": "MGIT-1.1"}, lander, notifyTestLogger())
	require.NoError(t, err)

	// Serve with a peer identity that does NOT match the binding.
	sock := serveOnePair(t, srv, "sbx-A", "peer-IMPOSTER")
	dialNotify(t, sock)

	calls, _ := waitForLands(lander, 1)
	assert.Equal(t, 0, calls, "a mismatched peer fails closed: no land")
}

// TestNotifyServer_TwoGuestIsolation_GuestACannotTriggerGuestBLand is the F-E
// regression: two distinct guests, each on its own per-VM listener bound to its
// own distinct host-observed identity. Guest A's notify triggers ONLY guest A's
// land; it can never reach guest B's listener or trigger guest B's task. With a
// non-distinct (constant "cid:3") identity, A could masquerade as B — this test
// guards against that. Refs: MGIT-11.10.11, SEC-10
func TestNotifyServer_TwoGuestIsolation_GuestACannotTriggerGuestBLand(t *testing.T) {
	binder := NewPeerBinder(notifyTestLogger())
	binder.Bind("sbx-A", "peer-A")
	binder.Bind("sbx-B", "peer-B")
	lander := &fakeNotifyLander{}
	srv, err := NewNotifyServer(binder,
		staticResolver{"sbx-A": "MGIT-A.1", "sbx-B": "MGIT-B.1"}, lander, notifyTestLogger())
	require.NoError(t, err)

	sockA := serveOnePair(t, srv, "sbx-A", "peer-A")
	_ = serveOnePair(t, srv, "sbx-B", "peer-B")

	// Only guest A signals. It must land ONLY task A.
	dialNotify(t, sockA)

	calls, tasks := waitForLands(lander, 1)
	require.Equal(t, 1, calls, "exactly one land — guest A's")
	assert.Equal(t, []string{"MGIT-A.1"}, tasks, "guest A cannot trigger guest B's land")
}

// TestNotifyServer_LandError_DoesNotCrash proves a failing triggered land is
// logged and dropped — one bad land never wedges the per-VM listener, which
// must keep serving later notifications.
func TestNotifyServer_LandError_DoesNotCrash(t *testing.T) {
	binder := NewPeerBinder(notifyTestLogger())
	binder.Bind("sbx-A", "peer-A")
	lander := &fakeNotifyLander{err: errors.New("land verification failed")}
	srv, err := NewNotifyServer(binder, staticResolver{"sbx-A": "MGIT-1.1"}, lander, notifyTestLogger())
	require.NoError(t, err)

	sock := serveOnePair(t, srv, "sbx-A", "peer-A")
	dialNotify(t, sock)
	dialNotify(t, sock)

	calls, _ := waitForLands(lander, 2)
	assert.Equal(t, 2, calls, "the listener keeps serving after a failed land")
}

// TestNewNotifyServer_NilDeps_Rejected proves the constructor rejects a nil
// dependency rather than serving an unverified trigger.
func TestNewNotifyServer_NilDeps_Rejected(t *testing.T) {
	binder := NewPeerBinder(notifyTestLogger())
	lander := &fakeNotifyLander{}
	res := staticResolver{}
	log := notifyTestLogger()
	tests := []struct {
		name   string
		binder *PeerBinder
		res    NotifyTaskResolver
		land   NotifyLander
		log    *slog.Logger
	}{
		{"nil_binder", nil, res, lander, log},
		{"nil_resolver", binder, nil, lander, log},
		{"nil_lander", binder, res, nil, log},
		{"nil_logger", binder, res, lander, nil},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := NewNotifyServer(tt.binder, tt.res, tt.land, tt.log)
			require.Error(t, err)
		})
	}
}
