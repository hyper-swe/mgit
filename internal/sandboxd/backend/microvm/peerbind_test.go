package microvm

import (
	"context"
	"log/slog"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/hyper-swe/mgit/internal/model"
)

// recordingBinder records Bind/Invalidate calls for assertions.
type recordingBinder struct {
	mu          sync.Mutex
	bound       map[string]string
	invalidated []string
}

func newRecordingBinder() *recordingBinder {
	return &recordingBinder{bound: map[string]string{}}
}
func (b *recordingBinder) Bind(sandboxID, peerID string) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.bound[sandboxID] = peerID
}
func (b *recordingBinder) Invalidate(sandboxID string) {
	b.mu.Lock()
	defer b.mu.Unlock()
	delete(b.bound, sandboxID)
	b.invalidated = append(b.invalidated, sandboxID)
}

// peerHypervisor returns VMs that report a fixed host-observed peer
// identity (a real AF_VSOCK CID, "cid:5").
type peerHypervisor struct{}

func (peerHypervisor) CreateVM(VMConfig) (VM, error) {
	return &peerVM{}, nil
}

type peerVM struct{ fakeVM }

func (*peerVM) PeerIdentity() string { return "cid:5" }

func managerWithBinder(t *testing.T, hv Hypervisor, binder PeerBinder) *Manager {
	t.Helper()
	images := testImages(t)
	mgr, err := NewManager(Config{
		Backend:    model.BackendKVM,
		WorkDir:    t.TempDir(),
		Resolve:    func(string) (ImagePaths, error) { return images, nil },
		Hypervisor: hv,
		PeerBinder: binder,
		Logger:     slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError})),
		Clock:      func() time.Time { return time.Date(2026, 6, 21, 12, 0, 0, 0, time.UTC) },
	})
	require.NoError(t, err)
	return mgr
}

// TestPeerBinding_VMReportsCID verifies a VM that reports a host-observed
// peer identity (the vsock CID) is bound under it at launch and
// invalidated at teardown (SEC-10).
func TestPeerBinding_VMReportsCID(t *testing.T) {
	binder := newRecordingBinder()
	mgr := managerWithBinder(t, peerHypervisor{}, binder)

	info, err := mgr.Launch(context.Background(), launchOpts("MGIT-1", model.NetworkModeNone))
	require.NoError(t, err)
	assert.Equal(t, "cid:5", binder.bound[info.ID], "the sandbox is bound to the VM's host-observed CID")

	require.NoError(t, mgr.Remove(context.Background(), info.ID, true))
	assert.NotContains(t, binder.bound, info.ID, "teardown invalidates the binding")
	assert.Equal(t, []string{info.ID}, binder.invalidated)
}

// TestPeerBinding_FallsBackToSandboxID verifies a VM that reports no peer
// identity is bound under its host-assigned sandbox ID.
func TestPeerBinding_FallsBackToSandboxID(t *testing.T) {
	binder := newRecordingBinder()
	mgr := managerWithBinder(t, &fakeHypervisor{}, binder)

	info, err := mgr.Launch(context.Background(), launchOpts("MGIT-2", model.NetworkModeNone))
	require.NoError(t, err)
	assert.Equal(t, info.ID, binder.bound[info.ID], "without a reported CID the sandbox ID is the peer identity")
}

// TestPeerBinding_NilBinder_NoPanic verifies binding is optional: a manager
// with no binder launches and tears down normally (the container fallback).
func TestPeerBinding_NilBinder_NoPanic(t *testing.T) {
	mgr := managerWithBinder(t, &fakeHypervisor{}, nil)
	info, err := mgr.Launch(context.Background(), launchOpts("MGIT-3", model.NetworkModeNone))
	require.NoError(t, err)
	require.NoError(t, mgr.Remove(context.Background(), info.ID, true))
}

// TestPeerBinding_RecycledIdentity_CannotInherit verifies that after a
// sandbox is torn down (binding invalidated), a successor reusing the same
// peer identity is bound under its OWN sandbox ID — the destroyed
// sandbox's channel cannot be inherited via a recycled CID (SEC-10).
func TestPeerBinding_RecycledIdentity_CannotInherit(t *testing.T) {
	binder := newRecordingBinder()
	mgr := managerWithBinder(t, peerHypervisor{}, binder) // both VMs report cid:5

	first, err := mgr.Launch(context.Background(), launchOpts("MGIT-4", model.NetworkModeNone))
	require.NoError(t, err)
	require.NoError(t, mgr.Remove(context.Background(), first.ID, true))
	assert.NotContains(t, binder.bound, first.ID, "the destroyed sandbox has no binding")

	second, err := mgr.Launch(context.Background(), launchOpts("MGIT-5", model.NetworkModeNone))
	require.NoError(t, err)
	assert.NotEqual(t, first.ID, second.ID, "the successor is a distinct sandbox")
	assert.Contains(t, binder.bound, second.ID, "only the live successor is bound")
	assert.NotContains(t, binder.bound, first.ID, "the recycled CID cannot inherit the destroyed sandbox")
}
