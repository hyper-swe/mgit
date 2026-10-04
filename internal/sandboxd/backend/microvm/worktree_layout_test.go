package microvm

import (
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/hyper-swe/mgit/internal/model"
)

// The backend answers registration's layout question with the provisioner's
// own shared store and the quarantine's own function, the ones its boot
// uses, so a worktree it would refuse at first boot is refused when it is
// registered instead (MGIT-222). No provisioner means no store to protect:
// no objection, as at boot. Refs: MGIT-222, SEC-03
func TestManager_CheckWorktreeLayout_AsksWhatTheBootAsks(t *testing.T) {
	repo := t.TempDir()
	shared := filepath.Join(repo, ".mgit")
	require.NoError(t, os.MkdirAll(shared, 0o750))
	mgr := quarantineManager(t, &fakeHypervisor{}, &fakeProvisioner{sharedDir: shared})

	err := mgr.CheckWorktreeLayout(repo)
	require.Error(t, err, "the repository root holds the shared store")
	assert.True(t, errors.Is(err, model.ErrSharedStoreReachable), "%v", err)
	assert.Zero(t, mgr.cfg.StoreProvisioner.(*fakeProvisioner).calls, "asking provisions nothing")
	assert.NoError(t, mgr.CheckWorktreeLayout(t.TempDir()), "a worktree outside the store is accepted")

	bare := quarantineManager(t, &fakeHypervisor{}, nil)
	assert.NoError(t, bare.CheckWorktreeLayout(repo), "with no store provisioner there is no store to protect")
}
