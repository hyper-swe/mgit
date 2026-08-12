package service

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/hyper-swe/mgit/internal/model"
)

// newSvcWithPolicy wires a service around an explicit host policy so the
// per-sandbox maxima under test are the ones the service enforces.
func newSvcWithPolicy(t *testing.T, mgr model.SandboxManager, p model.SandboxPolicy) *SandboxService {
	t.Helper()
	ids := 0
	svc, err := NewSandboxService(mgr, &fakeEventAppender{}, fakePolicy{p: p},
		func() time.Time { return time.Unix(0, 0).UTC() },
		func() (string, error) { ids++; return "01JXSBRES00000000000000000", nil })
	require.NoError(t, err)
	return svc
}

// TestRegister_DeclaredResources_ReachTheBackendLaunchOptions verifies a
// caller-declared cap survives registration and boot unchanged, so the value
// the workload asked for is the one the backend turns into a VMConfig.
// Refs: R-H212, FR-17.10
func TestRegister_DeclaredResources_ReachTheBackendLaunchOptions(t *testing.T) {
	mgr := &fakeSandboxManager{}
	svc := newSvcWithPolicy(t, mgr, model.DefaultSandboxPolicy())

	opts := regOpts("MGIT-95", "/work/declared")
	opts.CPUs, opts.MemoryMB, opts.DiskQuotaMB = 4, 6144, 20480

	info, err := svc.Register(context.Background(), opts)
	require.NoError(t, err)
	assert.Equal(t, 6144, info.MemoryMB, "the effective cap is visible from registration on")
	assert.Equal(t, 4, info.CPUs)
	assert.Equal(t, 20480, info.DiskQuotaMB)

	running, err := svc.EnsureRunning(context.Background(), "MGIT-95")
	require.NoError(t, err)
	assert.Equal(t, 6144, mgr.lastOpts.MemoryMB, "the declared memory reaches the backend")
	assert.Equal(t, 4, mgr.lastOpts.CPUs, "the declared CPU count reaches the backend")
	assert.Equal(t, 20480, mgr.lastOpts.DiskQuotaMB, "the declared disk quota reaches the backend")
	assert.Equal(t, 6144, running.MemoryMB, "a booted sandbox still reports its effective cap")
	assert.Equal(t, 4, running.CPUs)
	assert.Equal(t, 20480, running.DiskQuotaMB)
}

// TestRegister_UnsetResources_TakePolicyDefaults verifies an undeclared launch
// still gets the host policy defaults — the declarable surface must not change
// what a caller that declares nothing receives. Refs: R-H212, NFR-17.5
func TestRegister_UnsetResources_TakePolicyDefaults(t *testing.T) {
	mgr := &fakeSandboxManager{}
	p := model.DefaultSandboxPolicy()
	svc := newSvcWithPolicy(t, mgr, p)

	info, err := svc.Register(context.Background(), regOpts("MGIT-95.1", "/work/default"))
	require.NoError(t, err)
	assert.Equal(t, p.MemoryMB, info.MemoryMB)
	assert.Equal(t, p.CPUs, info.CPUs)
	assert.Equal(t, p.DiskQuotaMB, info.DiskQuotaMB)

	_, err = svc.EnsureRunning(context.Background(), "MGIT-95.1")
	require.NoError(t, err)
	assert.Equal(t, p.MemoryMB, mgr.lastOpts.MemoryMB, "the policy default reaches the backend")
	assert.Equal(t, p.CPUs, mgr.lastOpts.CPUs)
	assert.Equal(t, p.DiskQuotaMB, mgr.lastOpts.DiskQuotaMB)
	assert.Equal(t, p.TTL, mgr.lastOpts.TTL)
}

// TestRegister_OverPerSandboxMaximum_RefusedBeforeTheBackendSeesIt verifies an
// over-bound request is refused by the SERVICE — naming the limit and the
// policy field that set it — and that nothing was registered or launched. A
// silently clamped launch would reproduce the R-H212 defect one level up.
// Refs: R-H212
func TestRegister_OverPerSandboxMaximum_RefusedBeforeTheBackendSeesIt(t *testing.T) {
	p := model.DefaultSandboxPolicy()
	p.MaxMemoryMB = 4096
	mgr := &fakeSandboxManager{}
	svc := newSvcWithPolicy(t, mgr, p)

	opts := regOpts("MGIT-95.2", "/work/toobig")
	opts.MemoryMB = 8192

	_, err := svc.Register(context.Background(), opts)
	require.Error(t, err)
	assert.ErrorIs(t, err, model.ErrSandboxResourceLimitExceeded)
	assert.Contains(t, err.Error(), "4096", "the refusal names the limit")
	assert.Contains(t, err.Error(), "max_memory_mb", "the refusal names the policy that set it")
	assert.Zero(t, mgr.launches, "a refused request never reaches the backend")

	// Nothing was bound: the task is still free for a legal launch.
	_, err = svc.Status(context.Background(), "MGIT-95.2")
	assert.ErrorIs(t, err, model.ErrSandboxNotFound)

	opts.MemoryMB = 4096
	_, err = svc.Register(context.Background(), opts)
	assert.NoError(t, err, "a request at the maximum is legal")
}

// TestRegister_OverBoundAndFleetFull_RefusalsAreDistinguishable verifies the
// two ceilings are separately identifiable: the per-sandbox bound is a service
// refusal (ErrSandboxResourceLimitExceeded), the aggregate FR-17.26 ceiling is
// a backend-admission refusal (ErrSandboxCeilingExceeded), and neither
// masquerades as the other. "This launch is too big" and "the fleet is full"
// need different fixes. Refs: R-H212, FR-17.26
func TestRegister_OverBoundAndFleetFull_RefusalsAreDistinguishable(t *testing.T) {
	p := model.DefaultSandboxPolicy()
	p.MaxMemoryMB = 4096

	// The fleet-is-full refusal comes from the manager (the CeilingManager
	// decorator in production); a per-sandbox-legal request still hits it.
	mgr := &fakeSandboxManager{launchErr: model.ErrSandboxCeilingExceeded}
	svc := newSvcWithPolicy(t, mgr, p)

	legal := regOpts("MGIT-95.3", "/work/legal")
	legal.MemoryMB = 4096
	_, err := svc.Register(context.Background(), legal)
	require.NoError(t, err, "a per-sandbox-legal launch is admitted by the service")
	_, err = svc.EnsureRunning(context.Background(), "MGIT-95.3")
	require.Error(t, err)
	assert.ErrorIs(t, err, model.ErrSandboxCeilingExceeded, "the fleet-full refusal is the aggregate one")
	assert.NotErrorIs(t, err, model.ErrSandboxResourceLimitExceeded,
		"a full fleet must not read as an over-sized launch")

	over := regOpts("MGIT-95.4", "/work/over")
	over.MemoryMB = 8192
	_, err = svc.Register(context.Background(), over)
	require.Error(t, err)
	assert.ErrorIs(t, err, model.ErrSandboxResourceLimitExceeded)
	assert.NotErrorIs(t, err, model.ErrSandboxCeilingExceeded,
		"an over-sized launch must not read as a full fleet")
}
