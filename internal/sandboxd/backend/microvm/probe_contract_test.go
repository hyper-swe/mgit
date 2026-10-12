package microvm

import (
	"context"
	"path"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/hyper-swe/mgit/internal/model"
)

// The readiness probe names an absolute program, so no file the guest could
// resolve by name can answer it — the probe's proof is the guest's reply, and
// only the guest's control plane, never a file, should produce it.
// Refs: MGIT-272
func TestReadinessProbe_NamesAnAbsoluteProgram(t *testing.T) {
	require.NotEmpty(t, guestProbeCommand)
	assert.Truef(t, path.IsAbs(guestProbeCommand[0]),
		"the readiness probe names an absolute program: %q", guestProbeCommand[0])
}

// The probe's contract is preserved: the guest cannot run its program (it
// does not exist), so the guest replies with an error, and that reply is the
// proof the control channel is serving — the launch succeeds on it.
// Refs: MGIT-272, MGIT-92
func TestReadinessProbe_WhenTheGuestCannotRunIt_TheReplyStillProvesTheChannel(t *testing.T) {
	skipWithoutPOSIXShell(t)
	mgr := execManager(t, &pipeDialer{})
	info, err := mgr.Launch(context.Background(), launchOpts("MGIT-272", model.NetworkModeNone))
	require.NoError(t, err,
		"the guest's reply to a program it cannot run proves the channel serves, so the launch succeeds")
	require.NotEmpty(t, info.ID)
}
