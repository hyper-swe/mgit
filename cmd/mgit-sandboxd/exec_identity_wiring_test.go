package main

import (
	"os"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/hyper-swe/mgit/internal/model"
)

// The daemon runs guest execs as ITSELF — the uid/gid it delivered the
// worktree and the base as — and wires that identity into the service at
// build time, so no exec goes out without one. Refs: MGIT-151
func TestBuildSandboxService_WiresTheDaemonsOwnIdentityForExecs(t *testing.T) {
	clock := func() time.Time { return time.Date(2026, 9, 8, 12, 0, 0, 0, time.UTC) }
	hostRoot := t.TempDir()
	svc, _, closeAudit, err := buildSandboxService(nopManager{}, hostRoot, newPolicyStore(hostRoot, clock, testLogger()), clock)
	require.NoError(t, err)
	defer func() { _ = closeAudit() }()
	id := svc.ExecIdentity()
	require.NotNil(t, id, "an identity is wired at build time")
	assert.Equal(t, model.IdentityForProcess(os.Getuid(), os.Getgid()), *id,
		"the daemon's own identity by the one rule: root's own when root, the agent convention otherwise")
}
