// Package container tests: the reduced-isolation backend must run a guest
// command as the identity the daemon requests, the way the microVM backends
// do, so the identity model holds on every backend and the audit record
// matches what ran. Refs: MGIT-273, MGIT-151
package container

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/hyper-swe/mgit/internal/model"
)

// userArg returns the value of the podman "--user" flag in args, or "".
func userArg(args []string) string {
	for i, a := range args {
		if a == "--user" && i+1 < len(args) {
			return args[i+1]
		}
	}
	return ""
}

// TestContainer_Exec_AppliesRequestedIdentity verifies that a command run on
// the container backend runs as the identity the daemon requested: the podman
// exec invocation names that identity, and the result reports it. Refs: MGIT-273
func TestContainer_Exec_AppliesRequestedIdentity(t *testing.T) {
	runner := &fakeRunner{results: map[string]struct {
		out []byte
		err error
	}{
		"exec": {out: []byte("ok\n")},
	}}
	mgr := testManager(t, runner)
	ctx := context.Background()

	info, err := mgr.Launch(ctx, containerOpts(t, model.NetworkModeNone))
	require.NoError(t, err)

	id := model.IdentityForProcess(501, 20)
	res, err := mgr.Exec(ctx, info.ID, model.ExecRequest{Command: []string{"id", "-u"}, RunAs: &id})
	require.NoError(t, err)

	execCalls := runner.callsFor("exec")
	require.Len(t, execCalls, 1)
	assert.Equal(t, "501:20", userArg(execCalls[0]),
		"the podman exec invocation runs as the requested identity")

	require.NotNil(t, res.RanAs, "the result reports the identity it ran as")
	assert.Equal(t, 501, res.RanAs.UID)
	assert.Equal(t, 20, res.RanAs.GID)
}

// TestContainer_Exec_AuditedRootRequestChangesIdentity verifies that an
// audited request for a different identity on the container backend actually
// changes the identity the command runs as, rather than being recorded and
// ignored. Refs: MGIT-273, MGIT-151
func TestContainer_Exec_AuditedIdentityRequestChangesIdentity(t *testing.T) {
	runner := &fakeRunner{results: map[string]struct {
		out []byte
		err error
	}{
		"exec": {out: []byte("0\n")},
	}}
	mgr := testManager(t, runner)
	ctx := context.Background()

	info, err := mgr.Launch(ctx, containerOpts(t, model.NetworkModeNone))
	require.NoError(t, err)

	elevated := model.IdentityForProcess(0, 0)
	res, err := mgr.Exec(ctx, info.ID, model.ExecRequest{Command: []string{"id", "-u"}, RunAs: &elevated})
	require.NoError(t, err)

	execCalls := runner.callsFor("exec")
	require.Len(t, execCalls, 1)
	assert.Equal(t, "0:0", userArg(execCalls[0]),
		"an audited request for a different identity runs the command as it, not as the container's default")
	require.NotNil(t, res.RanAs)
	assert.Equal(t, 0, res.RanAs.UID, "the result reports the identity it ran as")
}
