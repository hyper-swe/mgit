package container

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// FEAT-6.114.1: the container backend delivers the guest repository by
// default, and not with NoGuestRepo.
func TestContainer_Quarantine_TheStagedWorktreeHasAGuestRepoByDefault(t *testing.T) {
	fx := setupQuarantineFixture(t)
	runner := &fakeRunner{}
	mgr := quarantinedManager(t, runner, fx)
	_, err := mgr.Launch(context.Background(), launchOpts(fx))
	require.NoError(t, err)

	src := runVolumeSource(t, runner.callsFor("run")[0], fx.wtPath)
	require.NotEmpty(t, src)
	assert.DirExists(t, filepath.Join(src, ".git"), "the guest is given a repository")
	assert.FileExists(t, filepath.Join(src, ".git", "HEAD"))
}

func TestContainer_Quarantine_NoGuestRepoDeliversNoRepo(t *testing.T) {
	fx := setupQuarantineFixture(t)
	runner := &fakeRunner{}
	mgr := quarantinedManager(t, runner, fx)
	opts := launchOpts(fx)
	opts.NoGuestRepo = true
	_, err := mgr.Launch(context.Background(), opts)
	require.NoError(t, err)

	src := runVolumeSource(t, runner.callsFor("run")[0], fx.wtPath)
	require.NotEmpty(t, src)
	assert.NoDirExists(t, filepath.Join(src, ".git"))
}
