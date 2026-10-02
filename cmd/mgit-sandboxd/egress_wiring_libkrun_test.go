//go:build cgo && !vzf && linux && libkrun

package main

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/hyper-swe/mgit/internal/model"
)

// allowlistManager boots a sandbox in allowlist mode, the shape a launch
// with a granted egress host takes, and reports it running.
type allowlistManager struct{ nopManager }

func (allowlistManager) Launch(_ context.Context, opts model.SandboxLaunchOptions) (*model.SandboxInfo, error) {
	return &model.SandboxInfo{
		ID: "01TESTALLOWLISTSANDBOX0000", TaskID: opts.TaskID,
		NetworkMode: model.NetworkModeAllowlist, NetworkAllowlist: opts.Network.Allowlist,
	}, nil
}

// The libkrun daemon enforces a sandbox's egress inside its own VM child; it
// creates no host tap. The firecracker egress controller binds its proxy and
// DNS on a per-sandbox tap gateway that only the firecracker backend creates,
// so installing it here made every allowlist sandbox fail at boot with "bind:
// cannot assign requested address" (the daemon in the 0.7.1 Linux archives
// did exactly that). The libkrun build must not install it, and an allowlist
// sandbox must boot through the service as the other modes do.
// Refs: MGIT-287, MGIT-72, ADR-010, SEC-04
func TestWireEgress_LibkrunBuild_InstallsNoHostTapController(t *testing.T) {
	clock := func() time.Time { return time.Unix(0, 0).UTC() }
	hostRoot := t.TempDir()
	svc, events, closeAudit, err := buildSandboxService(allowlistManager{}, hostRoot, newPolicyStore(hostRoot, clock, testLogger()), clock)
	require.NoError(t, err)
	t.Cleanup(func() { _ = closeAudit() })

	wired := wireEgress(svc, events, clock, testLogger())

	assert.Nil(t, wired.Grants, "capability escalation widens the daemon's own runner, which enforces nothing for a libkrun sandbox")
	assert.Nil(t, wired.Policy, "the live policy verbs act on the libkrun child, not the daemon's runner")
	_, err = svc.Register(context.Background(), model.SandboxLaunchOptions{
		TaskID: "MGIT-287", WorktreePath: "/work/a", ImageRef: "img@sha256:" + repeat64('a'),
		Network: model.NetworkPolicy{Mode: model.NetworkModeAllowlist, Allowlist: []string{"registry.npmjs.org"}},
	})
	require.NoError(t, err)

	_, err = svc.EnsureRunning(context.Background(), "MGIT-287")

	require.NoError(t, err, "an allowlist sandbox must boot without the daemon binding a tap gateway that does not exist")
}
