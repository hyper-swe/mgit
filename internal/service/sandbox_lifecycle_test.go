package service

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/hyper-swe/mgit/internal/model"
)

// mutableClock is a frozen clock the lifecycle tests advance by hand, so
// TTL expiry and idle-suspend thresholds are deterministic (no time.Now()).
type mutableClock struct{ t time.Time }

func (c *mutableClock) now() time.Time          { return c.t.UTC() }
func (c *mutableClock) advance(d time.Duration) { c.t = c.t.Add(d) }

// newLifecycleSvc builds a SandboxService over a caller-advanced clock so
// the lifecycle thresholds (TTL, idle) are exercised deterministically.
func newLifecycleSvc(t *testing.T, mgr model.SandboxManager, ev SandboxEventAppender, clk *mutableClock) *SandboxService {
	t.Helper()
	ids := 0
	svc, err := NewSandboxService(mgr, ev, fakePolicy{p: model.DefaultSandboxPolicy()},
		clk.now,
		func() (string, error) { ids++; return "01JXLC0000000000000000000" + string(rune('A'+ids)), nil })
	require.NoError(t, err)
	return svc
}

// bootedSvc registers and boots one sandbox for a task, returning the
// service, manager, event appender, and the booted sandbox info.
func bootedSvc(t *testing.T, clk *mutableClock, task, wt string) (*SandboxService, *fakeSandboxManager, *fakeEventAppender, *model.SandboxInfo) {
	t.Helper()
	mgr := &fakeSandboxManager{}
	ev := &fakeEventAppender{}
	svc := newLifecycleSvc(t, mgr, ev, clk)
	_, err := svc.Register(context.Background(), regOpts(task, wt))
	require.NoError(t, err)
	info, err := svc.EnsureRunning(context.Background(), task)
	require.NoError(t, err)
	return svc, mgr, ev, info
}

// TestLifecycle_IdleSuspend_ZeroCPU verifies that a sandbox idle past the
// idle threshold is suspended: the service calls the backend's Stop (pause,
// the host-side path to 0 CPU) without a force kill, records the suspended
// transition, and leaves the registration so the next exec resumes it.
//
// KVM-GATED: the literal "0 CPU on a real VM" assertion runs on real KVM
// hardware (e2e MGIT-11.13). Here we assert the host-side ORCHESTRATION:
// the service drives the backend pause after the idle threshold. Refs: NFR-17.3
func TestLifecycle_IdleSuspend_ZeroCPU(t *testing.T) {
	clk := &mutableClock{t: time.Unix(1_700_000_000, 0)}
	svc, mgr, ev, info := bootedSvc(t, clk, "MGIT-11.9.5", "/work/idle")
	idle := 10 * time.Minute

	// Not yet idle: a fresh boot is recent activity, so suspend is a no-op.
	suspended, err := svc.SuspendIdle(context.Background(), idle)
	require.NoError(t, err)
	assert.Empty(t, suspended, "a recently-active sandbox is not suspended")
	assert.Zero(t, mgr.stops, "no pause for an active sandbox")

	// Cross the idle threshold: the sandbox must be paused (Stop, not force).
	clk.advance(idle + time.Second)
	suspended, err = svc.SuspendIdle(context.Background(), idle)
	require.NoError(t, err)
	assert.Equal(t, []string{"MGIT-11.9.5"}, suspended, "the idle sandbox is suspended")
	assert.Equal(t, 1, mgr.stops, "the backend is paused (the host path to 0 CPU)")
	assert.Equal(t, info.ID, mgr.lastStopID)
	assert.False(t, mgr.lastStopForce, "idle-suspend pauses (graceful), it does not force-kill")
	assert.Zero(t, mgr.removes, "suspend pauses the VM; it does not tear it down")
	assert.Equal(t, []string{model.EventCreated, model.EventResumed, model.EventSuspended}, ev.types(),
		"the suspend transition is audited")

	// Idempotent: a second sweep does not re-suspend an already-suspended VM.
	clk.advance(idle)
	suspended, err = svc.SuspendIdle(context.Background(), idle)
	require.NoError(t, err)
	assert.Empty(t, suspended, "an already-suspended sandbox is not suspended again")
	assert.Equal(t, 1, mgr.stops)

	// A suspended sandbox resumes on the next exec (re-boot), audited resumed.
	_, err = svc.EnsureRunning(context.Background(), "MGIT-11.9.5")
	require.NoError(t, err)
	assert.Equal(t, []string{model.EventCreated, model.EventResumed, model.EventSuspended, model.EventResumed},
		ev.types(), "the resume after idle-suspend is audited")
	assert.Equal(t, 2, mgr.launches, "resume re-boots the paused VM")
}

// TestLifecycle_TTLExpiry_Reaped verifies that a sandbox past its TTL
// deadline is reaped: torn down through the backend (Stop+Remove) and a
// ttl_expired audit event emitted, and the task binding is freed. A
// sandbox still inside its TTL is left running. Refs: FR-17.9
func TestLifecycle_TTLExpiry_Reaped(t *testing.T) {
	clk := &mutableClock{t: time.Unix(1_700_000_000, 0)}
	ttl := model.DefaultSandboxPolicy().TTL
	svc, mgr, ev, info := bootedSvc(t, clk, "MGIT-11.9.5", "/work/ttl")

	// Inside the TTL: nothing is reaped.
	reaped, err := svc.ReapExpired(context.Background())
	require.NoError(t, err)
	assert.Empty(t, reaped, "a sandbox inside its TTL is not reaped")
	assert.Zero(t, mgr.removes)

	// Past the TTL deadline: the sandbox is reaped and audited ttl_expired.
	clk.advance(ttl + time.Minute)
	reaped, err = svc.ReapExpired(context.Background())
	require.NoError(t, err)
	assert.Equal(t, []string{"MGIT-11.9.5"}, reaped, "the expired sandbox is reaped")
	assert.Equal(t, 1, mgr.stops, "the expired VM is stopped")
	assert.Equal(t, 1, mgr.removes, "the expired VM is removed")
	assert.Equal(t, info.ID, mgr.lastRemoveID)
	assert.True(t, mgr.lastRemoveForce, "TTL reap force-tears-down (the deadline is hard)")
	assert.Equal(t, []string{model.EventCreated, model.EventResumed, model.EventTTLExpired}, ev.types(),
		"the TTL reap emits a ttl_expired audit event")

	// The task binding is freed: status is gone, the task can be re-registered.
	_, err = svc.Status(context.Background(), "MGIT-11.9.5")
	assert.ErrorIs(t, err, model.ErrSandboxNotFound)
	_, err = svc.Register(context.Background(), regOpts("MGIT-11.9.5", "/work/ttl"))
	assert.NoError(t, err, "the task can be re-registered after a TTL reap")
}

// TestLifecycle_TTLExpiry_UnbootedReaped verifies a registered-but-never-
// booted sandbox past its TTL is reaped without a backend call (there is no
// VM), still emitting ttl_expired. The unbooted TTL deadline runs from
// registration time. Refs: FR-17.9, FR-17.10
func TestLifecycle_TTLExpiry_UnbootedReaped(t *testing.T) {
	clk := &mutableClock{t: time.Unix(1_700_000_000, 0)}
	mgr := &fakeSandboxManager{}
	ev := &fakeEventAppender{}
	svc := newLifecycleSvc(t, mgr, ev, clk)
	_, err := svc.Register(context.Background(), regOpts("MGIT-11.9.5", "/work/lazy"))
	require.NoError(t, err)

	clk.advance(model.DefaultSandboxPolicy().TTL + time.Minute)
	reaped, err := svc.ReapExpired(context.Background())
	require.NoError(t, err)
	assert.Equal(t, []string{"MGIT-11.9.5"}, reaped)
	assert.Zero(t, mgr.stops, "an unbooted sandbox has no VM to stop")
	assert.Zero(t, mgr.removes)
	assert.Equal(t, []string{model.EventCreated, model.EventTTLExpired}, ev.types())
}

// TestLifecycle_LandThenTeardown verifies that landing a task's sandbox
// records the landed transition and then tears the sandbox down (Stop +
// Remove + destroyed), freeing the binding. Land is the success exit of a
// sandbox's life. Refs: FR-17.5, FR-17.9
func TestLifecycle_LandThenTeardown(t *testing.T) {
	clk := &mutableClock{t: time.Unix(1_700_000_000, 0)}
	svc, mgr, ev, info := bootedSvc(t, clk, "MGIT-11.9.5", "/work/land")

	require.NoError(t, svc.Land(context.Background(), "MGIT-11.9.5"))
	assert.Equal(t, 1, mgr.stops, "land tears the landed VM down")
	assert.Equal(t, 1, mgr.removes)
	assert.Equal(t, info.ID, mgr.lastRemoveID)
	assert.Equal(t, []string{model.EventCreated, model.EventResumed, model.EventLanded, model.EventDestroyed},
		ev.types(), "land audits landed then destroyed")

	// The binding is freed once the landed sandbox is torn down.
	_, err := svc.Status(context.Background(), "MGIT-11.9.5")
	assert.ErrorIs(t, err, model.ErrSandboxNotFound)
}

// TestLifecycle_LandUnknownTask_NotFound verifies landing an unregistered
// task fails closed.
func TestLifecycle_LandUnknownTask_NotFound(t *testing.T) {
	clk := &mutableClock{t: time.Unix(1_700_000_000, 0)}
	svc := newLifecycleSvc(t, &fakeSandboxManager{}, &fakeEventAppender{}, clk)
	assert.ErrorIs(t, svc.Land(context.Background(), "MGIT-nope"), model.ErrSandboxNotFound)
}

// TestLifecycle_LandAuditFailure_Surfaces verifies a failed landed-audit
// aborts the land before teardown (no VM is destroyed on an unrecorded
// land), leaving the sandbox registered for retry. Refs: FR-17.18
func TestLifecycle_LandAuditFailure_Surfaces(t *testing.T) {
	clk := &mutableClock{t: time.Unix(1_700_000_000, 0)}
	mgr := &fakeSandboxManager{}
	ev := &fakeEventAppender{failNth: 3} // created, resumed ok; landed fails
	svc := newLifecycleSvc(t, mgr, ev, clk)
	_, err := svc.Register(context.Background(), regOpts("MGIT-11.9.5", "/work/land"))
	require.NoError(t, err)
	_, err = svc.EnsureRunning(context.Background(), "MGIT-11.9.5")
	require.NoError(t, err)

	require.Error(t, svc.Land(context.Background(), "MGIT-11.9.5"))
	assert.Zero(t, mgr.removes, "no teardown when the land was not audited")
	_, err = svc.Status(context.Background(), "MGIT-11.9.5")
	assert.NoError(t, err, "an unrecorded land leaves the sandbox registered for retry")
}

// TestLifecycle_LandTeardownFailure_Surfaces verifies that when the landed
// audit succeeds but the VM teardown fails, the error surfaces and the
// sandbox stays registered for retry (the land is already recorded).
// Refs: FR-17.5, FR-17.9
func TestLifecycle_LandTeardownFailure_Surfaces(t *testing.T) {
	clk := &mutableClock{t: time.Unix(1_700_000_000, 0)}
	mgr := &fakeSandboxManager{removeErr: errors.New("vmm remove failed")}
	ev := &fakeEventAppender{}
	svc := newLifecycleSvc(t, mgr, ev, clk)
	_, err := svc.Register(context.Background(), regOpts("MGIT-11.9.5", "/work/land"))
	require.NoError(t, err)
	_, err = svc.EnsureRunning(context.Background(), "MGIT-11.9.5")
	require.NoError(t, err)

	require.Error(t, svc.Land(context.Background(), "MGIT-11.9.5"))
	assert.Equal(t, []string{model.EventCreated, model.EventResumed, model.EventLanded}, ev.types(),
		"the land is recorded even though teardown failed")
	_, err = svc.Status(context.Background(), "MGIT-11.9.5")
	assert.NoError(t, err, "a failed teardown leaves the landed sandbox registered for retry")
}

// TestLifecycle_TransitionsAudited drives a full lifecycle and asserts that
// EVERY transition appends an audit event in order: created (register),
// resumed (boot), suspended (idle), resumed (re-boot), then reaped at TTL
// (ttl_expired). No transition is silent. Refs: FR-17.18, NFR-17.3
func TestLifecycle_TransitionsAudited(t *testing.T) {
	clk := &mutableClock{t: time.Unix(1_700_000_000, 0)}
	mgr := &fakeSandboxManager{}
	ev := &fakeEventAppender{}
	svc := newLifecycleSvc(t, mgr, ev, clk)

	_, err := svc.Register(context.Background(), regOpts("MGIT-11.9.5", "/work/full"))
	require.NoError(t, err) // created

	_, err = svc.EnsureRunning(context.Background(), "MGIT-11.9.5")
	require.NoError(t, err) // resumed (boot)

	clk.advance(20 * time.Minute)
	_, err = svc.SuspendIdle(context.Background(), 10*time.Minute)
	require.NoError(t, err) // suspended

	_, err = svc.EnsureRunning(context.Background(), "MGIT-11.9.5")
	require.NoError(t, err) // resumed (re-boot)

	clk.advance(model.DefaultSandboxPolicy().TTL + time.Hour)
	_, err = svc.ReapExpired(context.Background())
	require.NoError(t, err) // ttl_expired

	assert.Equal(t, []string{
		model.EventCreated, model.EventResumed, model.EventSuspended,
		model.EventResumed, model.EventTTLExpired,
	}, ev.types(), "every lifecycle transition is audited, in order")

	// Each audited event names the sandbox and the bound task (host-observed
	// identity only, SEC-05 — never guest-asserted text).
	for _, e := range ev.events {
		assert.NotEmpty(t, e.SandboxID, "every transition records the host sandbox ID")
		assert.Equal(t, "MGIT-11.9.5", e.TaskID, "every transition records the bound task")
	}
}

// TestLifecycle_PruneAbandoned_RemovesExpired verifies the abandoned-prune
// sweep tears down every sandbox past its TTL across all tasks and audits
// each as ttl_expired, leaving live sandboxes untouched. Refs: FR-17.2, FR-17.9
func TestLifecycle_PruneAbandoned_RemovesExpired(t *testing.T) {
	clk := &mutableClock{t: time.Unix(1_700_000_000, 0)}
	mgr := &fakeSandboxManager{}
	ev := &fakeEventAppender{}
	svc := newLifecycleSvc(t, mgr, ev, clk)

	// Two booted sandboxes, both inside TTL.
	for _, tc := range []struct{ task, wt string }{{"MGIT-1.1", "/work/a"}, {"MGIT-1.2", "/work/b"}} {
		_, err := svc.Register(context.Background(), regOpts(tc.task, tc.wt))
		require.NoError(t, err)
		_, err = svc.EnsureRunning(context.Background(), tc.task)
		require.NoError(t, err)
	}

	pruned, err := svc.PruneAbandoned(context.Background())
	require.NoError(t, err)
	assert.Empty(t, pruned, "live sandboxes are not pruned")

	clk.advance(model.DefaultSandboxPolicy().TTL + time.Minute)
	pruned, err = svc.PruneAbandoned(context.Background())
	require.NoError(t, err)
	assert.ElementsMatch(t, []string{"MGIT-1.1", "MGIT-1.2"}, pruned, "both abandoned sandboxes are pruned")
	assert.Equal(t, 2, mgr.removes)
}

// TestLifecycle_SuspendIdle_BackendStopError_Surfaces verifies a pause
// failure surfaces and the sandbox stays booted (retryable next sweep).
func TestLifecycle_SuspendIdle_BackendStopError_Surfaces(t *testing.T) {
	clk := &mutableClock{t: time.Unix(1_700_000_000, 0)}
	mgr := &fakeSandboxManager{stopErr: errors.New("vmm pause hung")}
	ev := &fakeEventAppender{}
	svc := newLifecycleSvc(t, mgr, ev, clk)
	_, err := svc.Register(context.Background(), regOpts("MGIT-1", "/work/a"))
	require.NoError(t, err)
	_, err = svc.EnsureRunning(context.Background(), "MGIT-1")
	require.NoError(t, err)

	clk.advance(time.Hour)
	_, err = svc.SuspendIdle(context.Background(), time.Minute)
	require.Error(t, err)
	info, err := svc.Status(context.Background(), "MGIT-1")
	require.NoError(t, err)
	assert.Equal(t, model.StateRunning, info.State, "a failed pause leaves the sandbox running")
}

// TestLifecycle_SuspendIdle_AuditFailure_Surfaces verifies a failed
// suspended-audit surfaces (the VM is already paused; the registration is
// retryable). Refs: FR-17.18
func TestLifecycle_SuspendIdle_AuditFailure_Surfaces(t *testing.T) {
	clk := &mutableClock{t: time.Unix(1_700_000_000, 0)}
	mgr := &fakeSandboxManager{}
	ev := &fakeEventAppender{failNth: 3} // created, resumed ok; suspended fails
	svc := newLifecycleSvc(t, mgr, ev, clk)
	_, err := svc.Register(context.Background(), regOpts("MGIT-1", "/work/a"))
	require.NoError(t, err)
	_, err = svc.EnsureRunning(context.Background(), "MGIT-1")
	require.NoError(t, err)

	clk.advance(time.Hour)
	_, err = svc.SuspendIdle(context.Background(), time.Minute)
	require.Error(t, err)
}

// TestLifecycle_ReapExpired_BackendError_Surfaces verifies a teardown
// failure during reap surfaces and leaves the sandbox registered for the
// next sweep.
func TestLifecycle_ReapExpired_BackendError_Surfaces(t *testing.T) {
	clk := &mutableClock{t: time.Unix(1_700_000_000, 0)}
	mgr := &fakeSandboxManager{removeErr: errors.New("vmm remove failed")}
	ev := &fakeEventAppender{}
	svc := newLifecycleSvc(t, mgr, ev, clk)
	_, err := svc.Register(context.Background(), regOpts("MGIT-1", "/work/a"))
	require.NoError(t, err)
	_, err = svc.EnsureRunning(context.Background(), "MGIT-1")
	require.NoError(t, err)

	clk.advance(model.DefaultSandboxPolicy().TTL + time.Hour)
	_, err = svc.ReapExpired(context.Background())
	require.Error(t, err)
	_, statusErr := svc.Status(context.Background(), "MGIT-1")
	assert.NoError(t, statusErr, "a failed reap leaves the sandbox registered for retry")
}

// TestLifecycle_SuspendIdle_StopsEgress verifies idle-suspend releases the
// host egress controls (proxy/DNS) when a controller is wired, before the
// VM is paused. Refs: NFR-17.3, FR-17.8
func TestLifecycle_SuspendIdle_StopsEgress(t *testing.T) {
	clk := &mutableClock{t: time.Unix(1_700_000_000, 0)}
	mgr := &fakeSandboxManager{}
	ev := &fakeEventAppender{}
	eg := &fakeEgress{}
	svc := newLifecycleSvc(t, mgr, ev, clk)
	svc.SetEgressController(eg)

	opts := regOpts("MGIT-1.1", "/work/a")
	opts.Network = model.NetworkPolicy{Mode: model.NetworkModeAllowlist, Allowlist: []string{"registry.npmjs.org"}}
	reg, err := svc.Register(context.Background(), opts)
	require.NoError(t, err)
	_, err = svc.EnsureRunning(context.Background(), "MGIT-1.1")
	require.NoError(t, err)

	clk.advance(time.Hour)
	suspended, err := svc.SuspendIdle(context.Background(), time.Minute)
	require.NoError(t, err)
	assert.Equal(t, []string{"MGIT-1.1"}, suspended)
	assert.Equal(t, []string{reg.ID}, eg.stopped, "egress is released when the sandbox is suspended")
}

// TestLifecycle_ReapExpired_PolicyLoadFailure verifies a policy-load failure
// surfaces from the reap sweep (the effective TTL cannot be resolved).
func TestLifecycle_ReapExpired_PolicyLoadFailure(t *testing.T) {
	clk := &mutableClock{t: time.Unix(1_700_000_000, 0)}
	svc, err := NewSandboxService(&fakeSandboxManager{}, &fakeEventAppender{}, errPolicy{},
		clk.now, func() (string, error) { return "01JXLC0000000000000000000Z", nil })
	require.NoError(t, err)
	_, err = svc.ReapExpired(context.Background())
	assert.Error(t, err)
}

// TestLifecycle_NoTTL_NeverReaped verifies a sandbox launched with TTL=0
// (policy default disabled) is never TTL-reaped — a zero deadline means no
// deadline. Refs: FR-17.9
func TestLifecycle_NoTTL_NeverReaped(t *testing.T) {
	clk := &mutableClock{t: time.Unix(1_700_000_000, 0)}
	mgr := &fakeSandboxManager{}
	ev := &fakeEventAppender{}
	// A policy with TTL disabled.
	pol := model.DefaultSandboxPolicy()
	pol.TTL = 0
	ids := 0
	svc, err := NewSandboxService(mgr, ev, fakePolicy{p: pol}, clk.now,
		func() (string, error) { ids++; return "01JXLC0000000000000000000" + string(rune('A'+ids)), nil })
	require.NoError(t, err)

	_, err = svc.Register(context.Background(), regOpts("MGIT-1", "/work/a"))
	require.NoError(t, err)
	_, err = svc.EnsureRunning(context.Background(), "MGIT-1")
	require.NoError(t, err)

	clk.advance(100 * time.Hour)
	reaped, err := svc.ReapExpired(context.Background())
	require.NoError(t, err)
	assert.Empty(t, reaped, "a sandbox with no TTL is never reaped")
}
