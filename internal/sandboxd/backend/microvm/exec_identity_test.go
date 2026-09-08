package microvm

import (
	"context"
	"os"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/hyper-swe/mgit/internal/model"
)

// The identity the guest reports running the command as reaches the
// manager's result untouched, so the service can judge it: with a real
// supervisor on the other end of the pipe and nothing asked for, the echo
// is the supervisor's own uid/gid. Refs: MGIT-151
func TestExec_CarriesTheIdentityTheGuestRanAs(t *testing.T) {
	skipWithoutPOSIXShell(t)
	mgr := execManager(t, &pipeDialer{})
	ctx := context.Background()
	info, err := mgr.Launch(ctx, launchOpts("MGIT-151", model.NetworkModeNone))
	require.NoError(t, err)
	res, err := mgr.Exec(ctx, info.ID, model.ExecRequest{Command: []string{"/bin/sh", "-c", "true"}})
	require.NoError(t, err)
	require.NotNil(t, res.RanAs, "the guest's echo is carried, never dropped")
	assert.Equal(t, os.Getuid(), res.RanAs.UID)
	assert.Equal(t, os.Getgid(), res.RanAs.GID)
}
