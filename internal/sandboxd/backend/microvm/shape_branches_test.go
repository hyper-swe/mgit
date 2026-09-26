package microvm

import (
	"context"
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/hyper-swe/mgit/internal/model"
)

// shapeManager is a manager for backend whose image always resolves to images.
func shapeManager(t *testing.T, backend string, images ImagePaths) (*Manager, *fakeHypervisor) {
	t.Helper()
	hv := &fakeHypervisor{}
	mgr, err := NewManager(Config{
		Backend:    backend,
		WorkDir:    t.TempDir(),
		Resolve:    func(string) (ImagePaths, error) { return images, nil },
		Hypervisor: hv,
		Logger:     slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError})),
		Clock:      func() time.Time { return time.Date(2026, 9, 24, 12, 0, 0, 0, time.UTC) },
	})
	require.NoError(t, err)
	return mgr, hv
}

// A root the check cannot stat is not a shape it can judge. It claims only
// what it can see and leaves the launch to report the missing path, rather
// than refusing a base it knows nothing about. Refs: MGIT-233.2, MGIT-233
func TestManager_Launch_ARootThatCannotBeStatedIsLeftToTheLaunch(t *testing.T) {
	missing := ImagePaths{KernelPath: "/nope/vmlinux", RootfsPath: filepath.Join(t.TempDir(), "gone")}
	for _, backend := range []string{model.BackendKVM, model.BackendVZF, model.BackendLibkrun} {
		t.Run(backend, func(t *testing.T) {
			mgr, hv := shapeManager(t, backend, missing)
			_, err := mgr.Launch(context.Background(), launchOpts("T-2", model.NetworkModeNone))
			assert.False(t, errors.Is(err, model.ErrGuestBaseUnbootable), "not judged: %v", err)
			assert.Len(t, hv.configs, 1, "the launch goes on and reports the path itself")
		})
	}
}

// The way out differs by host. A firecracker daemon is the Linux go-install
// build, and the Linux release archive's libkrun daemon boots the directory
// as it is. vzf is the macOS -tags vzf build, whose way out is the release's
// daemon, not a Linux archive. Refs: MGIT-233.2, MGIT-233
func TestManager_Launch_EachImageBackendNamesItsOwnWayOut(t *testing.T) {
	dir := ImagePaths{RootfsPath: t.TempDir()}

	mgr, _ := shapeManager(t, model.BackendKVM, dir)
	_, err := mgr.Launch(context.Background(), launchOpts("T-3", model.NetworkModeNone))
	require.ErrorIs(t, err, model.ErrGuestBaseUnbootable)
	assert.Contains(t, err.Error(), "the Linux release archive's daemon", "firecracker's way out is the Linux archive")

	mgr, _ = shapeManager(t, model.BackendVZF, dir)
	_, err = mgr.Launch(context.Background(), launchOpts("T-4", model.NetworkModeNone))
	require.ErrorIs(t, err, model.ErrGuestBaseUnbootable)
	assert.NotContains(t, err.Error(), "Linux release archive", "a macOS vzf build is not sent to a Linux archive")
}
