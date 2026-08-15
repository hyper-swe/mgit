package service

import (
	"context"
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/hyper-swe/mgit/internal/model"
)

// allowlistOpts registers an allowlist-mode sandbox — the only mode with a
// mutable policy (none has no egress, open has no allowlist).
func allowlistOpts(task, wt string, allow ...string) model.SandboxLaunchOptions {
	opts := regOpts(task, wt)
	opts.Network = model.NetworkPolicy{Mode: model.NetworkModeAllowlist, Allowlist: allow}
	return opts
}

// TestSandboxService_StagePendingEgressPolicy_NotBooted_VMBootsWithIt is the
// half of MGIT-109 that turns a plausible fix into a correct one: staging is
// only "not a weakening" if the staged policy is genuinely what the VM launches
// under. This asserts it at the seam the backend actually reads — the launch
// options handed to Launch. Refs: MGIT-109, FR-17.10, SEC-04
func TestSandboxService_StagePendingEgressPolicy_NotBooted_VMBootsWithIt(t *testing.T) {
	mgr := &fakeSandboxManager{}
	svc := newSvc(t, mgr, &fakeEventAppender{})
	ctx := context.Background()
	info, err := svc.Register(ctx, allowlistOpts("MGIT-109", "/wt", "example.com:443"))
	require.NoError(t, err)

	state, err := svc.StagePendingEgressPolicy(ctx, info.ID, []string{"other.example.com:443"})
	require.NoError(t, err)
	assert.Equal(t, []string{"other.example.com:443"}, state.Entries)
	assert.True(t, state.Pending, "nothing is enforcing this yet")
	assert.Equal(t, 0, mgr.launches, "staging must not boot a VM merely to configure it")

	booted, err := svc.EnsureRunning(ctx, "MGIT-109")
	require.NoError(t, err)
	assert.Equal(t, []string{"other.example.com:443"}, mgr.lastOpts.Network.Allowlist,
		"the VM must launch under the STAGED allowlist, never the replaced launch-time one")
	assert.Equal(t, []string{"other.example.com:443"}, booted.NetworkAllowlist)
}

// TestSandboxService_StagePendingEgressPolicy_Booted_ReturnsErrSandboxBooted is
// the race the sentinel exists for. A boot that lands between the caller
// reading the recorded state and this call must refuse — having staged NOTHING,
// so a retry through the live enforcer cannot double-apply — rather than let a
// staged policy be reported as an enforced one. Refs: MGIT-109, MGIT-72, SEC-04
func TestSandboxService_StagePendingEgressPolicy_Booted_ReturnsErrSandboxBooted(t *testing.T) {
	mgr := &fakeSandboxManager{}
	svc := newSvc(t, mgr, &fakeEventAppender{})
	ctx := context.Background()
	info, err := svc.Register(ctx, allowlistOpts("MGIT-109", "/wt", "example.com:443"))
	require.NoError(t, err)
	_, err = svc.EnsureRunning(ctx, "MGIT-109")
	require.NoError(t, err)

	_, err = svc.StagePendingEgressPolicy(ctx, info.ID, []string{"other.example.com:443"})

	require.ErrorIs(t, err, model.ErrSandboxBooted)
	got, err := svc.PendingEgressPolicy(ctx, info.ID)
	require.ErrorIs(t, err, model.ErrSandboxBooted)
	assert.Empty(t, got.Entries)

	// Nothing was staged: the running VM's options are untouched, so the
	// caller's re-route to the live enforcer is the ONLY change that lands.
	sb, err := svc.Status(ctx, "MGIT-109")
	require.NoError(t, err)
	assert.Equal(t, []string{"example.com:443"}, sb.NetworkAllowlist)
}

// TestSandboxService_StagePendingEgressPolicy_Suspended_StagesForResume covers
// the other not-booted state: idle-suspend stops the VM and leaves the
// registration to re-launch from these same options, so a suspended sandbox is
// stageable and the resume enforces what was staged. Refs: MGIT-109, NFR-17.3
func TestSandboxService_StagePendingEgressPolicy_Suspended_StagesForResume(t *testing.T) {
	mgr := &fakeSandboxManager{}
	svc := newSvc(t, mgr, &fakeEventAppender{})
	ctx := context.Background()
	info, err := svc.Register(ctx, allowlistOpts("MGIT-109", "/wt", "example.com:443"))
	require.NoError(t, err)
	_, err = svc.EnsureRunning(ctx, "MGIT-109")
	require.NoError(t, err)
	_, err = svc.SuspendIdle(ctx, 0)
	require.NoError(t, err)

	_, err = svc.StagePendingEgressPolicy(ctx, info.ID, []string{"other.example.com:443"})
	require.NoError(t, err)

	_, err = svc.EnsureRunning(ctx, "MGIT-109")
	require.NoError(t, err)
	assert.Equal(t, []string{"other.example.com:443"}, mgr.lastOpts.Network.Allowlist,
		"the resumed VM enforces what was staged while it was paused")
}

// TestSandboxService_StagePendingEgressPolicy_UnknownSandbox_NotFound refuses a
// sandbox ID that resolves to nothing rather than staging into the void.
// Refs: MGIT-109, FR-17.20
func TestSandboxService_StagePendingEgressPolicy_UnknownSandbox_NotFound(t *testing.T) {
	svc := newSvc(t, &fakeSandboxManager{}, &fakeEventAppender{})

	_, err := svc.StagePendingEgressPolicy(context.Background(), "01NOPE", nil)
	require.ErrorIs(t, err, model.ErrSandboxNotFound)

	_, err = svc.PendingEgressPolicy(context.Background(), "01NOPE")
	require.ErrorIs(t, err, model.ErrSandboxNotFound)
}

// TestSandboxService_PendingEgressPolicy_NotBooted_ReportsWhatWillBeEnforced is
// the read side: `policy show` on a registered-but-unbooted sandbox reports the
// launch allowlist it WILL come up with, flagged pending. Reporting an empty
// policy instead would read as "nothing is permitted" when the truth is
// "nothing is enforcing yet". Refs: MGIT-109, MGIT-72, SEC-04
func TestSandboxService_PendingEgressPolicy_NotBooted_ReportsWhatWillBeEnforced(t *testing.T) {
	svc := newSvc(t, &fakeSandboxManager{}, &fakeEventAppender{})
	ctx := context.Background()
	info, err := svc.Register(ctx, allowlistOpts("MGIT-109", "/wt", "example.com:443"))
	require.NoError(t, err)

	got, err := svc.PendingEgressPolicy(ctx, info.ID)

	require.NoError(t, err)
	assert.Equal(t, []string{"example.com:443"}, got.Entries)
	assert.True(t, got.Pending)
	assert.Zero(t, got.RuleCount, "no enforcer has compiled these entries")
}

// TestSandboxService_StagePendingEgressPolicy_IsDurable proves the stage
// survives a daemon restart. Without the durable write, a daemon that exited
// before first use would rehydrate the registration from its LAUNCH-TIME
// allowlist and boot the VM under the wider policy the caller had replaced —
// a silent fail-open at exactly the moment containment is relied on.
// Refs: MGIT-109, MGIT-102, SEC-04
func TestSandboxService_StagePendingEgressPolicy_IsDurable(t *testing.T) {
	reg := newFakeRegistry()
	svc := newSvc(t, &fakeSandboxManager{}, &fakeEventAppender{})
	svc.SetRegistry(reg)
	ctx := context.Background()
	info, err := svc.Register(ctx, allowlistOpts("MGIT-109", "/wt", "example.com:443"))
	require.NoError(t, err)

	_, err = svc.StagePendingEgressPolicy(ctx, info.ID, []string{"other.example.com:443"})
	require.NoError(t, err)

	// A fresh service adopting the persisted rows is the daemon restart.
	next := newSvc(t, &fakeSandboxManager{}, &fakeEventAppender{})
	next.SetRegistry(reg)
	_, err = next.Rehydrate(ctx)
	require.NoError(t, err)
	got, err := next.PendingEgressPolicy(ctx, info.ID)
	require.NoError(t, err)
	assert.Equal(t, []string{"other.example.com:443"}, got.Entries,
		"a restart must not resurrect the replaced launch-time allowlist")
}

// TestSandboxService_StagePendingEgressPolicy_RegistryFails_StagesNothing fails
// closed at the durability seam: a stage that could not be recorded must not be
// reported as staged, or the caller runs untrusted code believing a policy is
// waiting that a restart would discard. Refs: MGIT-109, SEC-04
func TestSandboxService_StagePendingEgressPolicy_RegistryFails_StagesNothing(t *testing.T) {
	reg := newFakeRegistry()
	svc := newSvc(t, &fakeSandboxManager{}, &fakeEventAppender{})
	svc.SetRegistry(reg)
	ctx := context.Background()
	info, err := svc.Register(ctx, allowlistOpts("MGIT-109", "/wt", "example.com:443"))
	require.NoError(t, err)
	reg.upsertErr = errors.New("disk full")

	_, err = svc.StagePendingEgressPolicy(ctx, info.ID, []string{"other.example.com:443"})

	require.Error(t, err)
	got, err := svc.PendingEgressPolicy(ctx, info.ID)
	require.NoError(t, err)
	assert.Equal(t, []string{"example.com:443"}, got.Entries,
		"the in-memory options must not diverge from what was durably recorded")
}
