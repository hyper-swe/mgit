package microvm

import (
	"context"
	"errors"
	"log/slog"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/hyper-swe/mgit/internal/model"
)

// managerWithNotify builds a manager wired with a NotifyRegistrar (and no peer
// binder needed for these assertions).
func managerWithNotify(t *testing.T, hv Hypervisor, reg NotifyRegistrar) *Manager {
	t.Helper()
	images := testImages(t)
	mgr, err := NewManager(Config{
		Backend:         model.BackendKVM,
		WorkDir:         t.TempDir(),
		Resolve:         func(string) (ImagePaths, error) { return images, nil },
		Hypervisor:      hv,
		NotifyRegistrar: reg,
		Logger:          slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError})),
		Clock:           func() time.Time { return time.Date(2026, 6, 21, 12, 0, 0, 0, time.UTC) },
	})
	require.NoError(t, err)
	return mgr
}

// recordingNotifyReg records Register/Deregister calls for assertions.
type recordingNotifyReg struct {
	mu          sync.Mutex
	registered  map[string]notifyRec
	deregged    []string
	registerErr error
}

type notifyRec struct{ taskID, peerID, socketPath string }

func newRecordingNotifyReg() *recordingNotifyReg {
	return &recordingNotifyReg{registered: map[string]notifyRec{}}
}

func (r *recordingNotifyReg) Register(sandboxID, taskID, peerID, socketPath string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.registerErr != nil {
		return r.registerErr
	}
	r.registered[sandboxID] = notifyRec{taskID: taskID, peerID: peerID, socketPath: socketPath}
	return nil
}

func (r *recordingNotifyReg) Deregister(sandboxID string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	delete(r.registered, sandboxID)
	r.deregged = append(r.deregged, sandboxID)
}

// notifyHypervisor returns VMs that report a per-VM peer identity AND a notify
// socket path (the firecracker shape), so the manager wires the auto-land
// trigger.
type notifyHypervisor struct{}

func (notifyHypervisor) CreateVM(cfg VMConfig) (VM, error) {
	return &notifyVMStub{id: cfg.SandboxID}, nil
}

type notifyVMStub struct {
	fakeVM
	id string
}

func (v *notifyVMStub) PeerIdentity() string { return "peer-" + v.id }
func (v *notifyVMStub) NotifySocketPath() string {
	return "/work/" + v.id + "/vsock.sock_1026"
}

// TestNotifyRegistrar_RegisteredAtLaunch_DeregisteredAtTeardown proves the
// manager starts the per-VM notify listener at launch (with the host-bound
// task, the VM's per-VM peer identity, and its notify socket) and stops it at
// teardown. Refs: MGIT-11.10.11, SEC-10
func TestNotifyRegistrar_RegisteredAtLaunch_DeregisteredAtTeardown(t *testing.T) {
	reg := newRecordingNotifyReg()
	mgr := managerWithNotify(t, notifyHypervisor{}, reg)

	info, err := mgr.Launch(context.Background(), launchOpts("MGIT-1", model.NetworkModeNone))
	require.NoError(t, err)

	rec, ok := reg.registered[info.ID]
	require.True(t, ok, "the per-VM notify listener is registered at launch")
	assert.Equal(t, "MGIT-1", rec.taskID, "registered with the host-bound task")
	assert.Equal(t, "peer-"+info.ID, rec.peerID, "registered with the VM's per-VM peer identity (F-E)")
	assert.Equal(t, "/work/"+info.ID+"/vsock.sock_1026", rec.socketPath)

	require.NoError(t, mgr.Remove(context.Background(), info.ID, true))
	assert.NotContains(t, reg.registered, info.ID, "teardown deregisters the listener")
	assert.Equal(t, []string{info.ID}, reg.deregged)
}

// TestNotifyRegistrar_VMWithoutNotifySocket_NotRegistered proves a VM that
// exposes no notify socket (a backend without the guest->host path) does not
// wire the trigger — the host-initiated land path is unaffected.
func TestNotifyRegistrar_VMWithoutNotifySocket_NotRegistered(t *testing.T) {
	reg := newRecordingNotifyReg()
	mgr := managerWithNotify(t, peerHypervisor{}, reg) // peerVM has no NotifySocketPath

	info, err := mgr.Launch(context.Background(), launchOpts("MGIT-1", model.NetworkModeNone))
	require.NoError(t, err)
	assert.NotContains(t, reg.registered, info.ID, "no notify socket -> no trigger wired")
}

// TestNotifyRegistrar_RegisterError_NonFatal proves a listen failure at launch
// does not fail the launch — the sandbox runs and the host-initiated land still
// works; only auto-land is unavailable. Refs: MGIT-11.10.11
func TestNotifyRegistrar_RegisterError_NonFatal(t *testing.T) {
	reg := newRecordingNotifyReg()
	reg.registerErr = errors.New("listen refused")
	mgr := managerWithNotify(t, notifyHypervisor{}, reg)

	info, err := mgr.Launch(context.Background(), launchOpts("MGIT-1", model.NetworkModeNone))
	require.NoError(t, err, "a notify listen failure must not fail the launch")
	require.NoError(t, mgr.Remove(context.Background(), info.ID, true))
}

// TestNotifyRegistrar_Nil_NoPanic proves the trigger is optional: a manager
// with no registrar launches and tears down normally.
func TestNotifyRegistrar_Nil_NoPanic(t *testing.T) {
	mgr := managerWithNotify(t, notifyHypervisor{}, nil)
	info, err := mgr.Launch(context.Background(), launchOpts("MGIT-1", model.NetworkModeNone))
	require.NoError(t, err)
	require.NoError(t, mgr.Remove(context.Background(), info.ID, true))
}
