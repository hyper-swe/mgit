package microvm

import (
	"context"
	"errors"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/hyper-swe/mgit/internal/model"
)

// A sandbox that is not running is refused by exec, export and sync, and the
// refusal must state what was verified: THIS sandbox is not running. It was
// wrapped in ErrSandboxBackendUnavailable, whose text ("no sandbox backend
// available on this platform") sends the reader to install a hypervisor
// that is already installed and linked. Refs: MGIT-232
func TestManager_NotRunning_IsSaidAsThatNotAsAMissingBackend(t *testing.T) {
	mgr, workDir := testManager(t, &fakeHypervisor{})
	id, _ := launchWithStagedTree(t, mgr, workDir, "MGIT-232")
	require.NoError(t, mgr.Stop(context.Background(), id, false))

	for name, call := range map[string]func() error{
		"exec": func() error {
			_, err := mgr.Exec(context.Background(), id, model.ExecRequest{Command: []string{"/bin/true"}})
			return err
		},
		"export": func() error {
			_, err := mgr.ExportArtifact(context.Background(), id,
				model.ArtifactExportRequest{GuestPath: "out", HostPath: filepath.Join(t.TempDir(), "a")})
			return err
		},
		"sync": func() error {
			_, err := mgr.SyncWorktree(context.Background(), id, model.WorktreeSyncOptions{})
			return err
		},
	} {
		t.Run(name, func(t *testing.T) {
			err := call()
			require.ErrorIs(t, err, model.ErrSandboxNotRunning)
			assert.False(t, errors.Is(err, model.ErrSandboxBackendUnavailable), "the backend is available: %v", err)
			assert.NotContains(t, err.Error(), "no sandbox backend available")
			assert.Contains(t, err.Error(), string(model.StateSuspended), "the state it is in is named")
		})
	}
}
