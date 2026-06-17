package service

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/hyper-swe/mgit/internal/model"
)

// fakeSandboxManager records Launch calls and echoes opts into the info.
type fakeSandboxManager struct {
	launches  int
	lastOpts  model.SandboxLaunchOptions
	launchErr error
}

func (m *fakeSandboxManager) Launch(_ context.Context, opts model.SandboxLaunchOptions) (*model.SandboxInfo, error) {
	m.launches++
	m.lastOpts = opts
	if m.launchErr != nil {
		return nil, m.launchErr
	}
	return &model.SandboxInfo{
		ID: opts.SandboxID, TaskID: opts.TaskID, WorktreePath: opts.WorktreePath,
		Backend: model.BackendKVM, State: model.StateRunning, MemoryMB: opts.MemoryMB,
	}, nil
}
func (m *fakeSandboxManager) List(context.Context) ([]model.SandboxInfo, error) { return nil, nil }
func (m *fakeSandboxManager) Exec(context.Context, string, model.ExecRequest) (*model.ExecResult, error) {
	return nil, nil
}
func (m *fakeSandboxManager) Stop(context.Context, string, bool) error   { return nil }
func (m *fakeSandboxManager) Remove(context.Context, string, bool) error { return nil }
func (m *fakeSandboxManager) Resolve(context.Context, string) (*model.SandboxInfo, error) {
	return nil, nil
}

// fakeEventAppender records appended sandbox events.
type fakeEventAppender struct {
	events  []model.SandboxEvent
	failNth int // 1-based; 0 = never
}

func (a *fakeEventAppender) AppendSandboxEvent(_ context.Context, ev *model.SandboxEvent) error {
	if a.failNth != 0 && len(a.events)+1 == a.failNth {
		return errors.New("audit write failed")
	}
	a.events = append(a.events, *ev)
	return nil
}
func (a *fakeEventAppender) types() []string {
	out := make([]string, len(a.events))
	for i, e := range a.events {
		out[i] = e.EventType
	}
	return out
}

// fakePolicy returns a fixed effective policy.
type fakePolicy struct{ p model.SandboxPolicy }

func (f fakePolicy) Load(context.Context) (model.SandboxPolicy, error) { return f.p, nil }

func testImageRef() string {
	return "go-node@sha256:" + strings.Repeat("a", 64)
}

func newSvc(t *testing.T, mgr model.SandboxManager, ev SandboxEventAppender) *SandboxService {
	t.Helper()
	ids := 0
	svc, err := NewSandboxService(mgr, ev, fakePolicy{p: model.DefaultSandboxPolicy()},
		func() time.Time { return time.Unix(0, 0).UTC() },
		func() (string, error) { ids++; return "01JXSB" + strings.Repeat("0", 20) + string(rune('A'+ids)), nil })
	require.NoError(t, err)
	return svc
}

func regOpts(task, wt string) model.SandboxLaunchOptions {
	return model.SandboxLaunchOptions{
		TaskID: task, WorktreePath: wt, ImageRef: testImageRef(),
		Network: model.NetworkPolicy{Mode: model.NetworkModeNone},
	}
}

// TestProvision_LazyNoBootUntilExec verifies registration does not boot
// the VM; the first EnsureRunning boots it; a second does not re-boot.
// Refs: FR-17.10
func TestProvision_LazyNoBootUntilExec(t *testing.T) {
	mgr := &fakeSandboxManager{}
	ev := &fakeEventAppender{}
	svc := newSvc(t, mgr, ev)

	info, err := svc.Register(context.Background(), regOpts("MGIT-11.9.1", "/work/a"))
	require.NoError(t, err)
	assert.Equal(t, model.StateCreated, info.State)
	assert.Zero(t, mgr.launches, "registration must not boot the VM")
	assert.Equal(t, []string{model.EventCreated}, ev.types(), "registration is audited as created")

	running, err := svc.EnsureRunning(context.Background(), "MGIT-11.9.1")
	require.NoError(t, err)
	assert.Equal(t, model.StateRunning, running.State)
	assert.Equal(t, 1, mgr.launches, "first exec boots the VM")

	_, err = svc.EnsureRunning(context.Background(), "MGIT-11.9.1")
	require.NoError(t, err)
	assert.Equal(t, 1, mgr.launches, "an already-running sandbox is not re-booted")
	assert.Equal(t, []string{model.EventCreated, model.EventResumed}, ev.types())
}

// TestProvision_DuplicateTask_Rejected verifies one-task/one-worktree
// exclusivity. Refs: FR-17.1
func TestProvision_DuplicateTask_Rejected(t *testing.T) {
	svc := newSvc(t, &fakeSandboxManager{}, &fakeEventAppender{})
	_, err := svc.Register(context.Background(), regOpts("MGIT-1", "/work/a"))
	require.NoError(t, err)

	_, err = svc.Register(context.Background(), regOpts("MGIT-1", "/work/b"))
	assert.ErrorIs(t, err, model.ErrTaskAlreadyBound, "a second sandbox for the same task is rejected")

	_, err = svc.Register(context.Background(), regOpts("MGIT-2", "/work/a"))
	assert.ErrorIs(t, err, model.ErrTaskAlreadyBound, "a second sandbox for the same worktree is rejected")
}

// TestProvision_CommitInheritsTaskID verifies the sandbox is bound to the
// task (so commits made in it inherit that task ID), and that the
// host-assigned sandbox ID is the one the backend boots with. Refs: FR-17.1
func TestProvision_CommitInheritsTaskID(t *testing.T) {
	mgr := &fakeSandboxManager{}
	svc := newSvc(t, mgr, &fakeEventAppender{})

	reg, err := svc.Register(context.Background(), regOpts("MGIT-7.3", "/work/x"))
	require.NoError(t, err)
	running, err := svc.EnsureRunning(context.Background(), "MGIT-7.3")
	require.NoError(t, err)

	assert.Equal(t, "MGIT-7.3", running.TaskID, "the sandbox is bound to the task")
	assert.Equal(t, reg.ID, mgr.lastOpts.SandboxID, "the backend boots with the host-assigned ID")
	assert.Equal(t, "MGIT-7.3", mgr.lastOpts.TaskID)
}

// TestProvision_PolicyDefaultsApplied verifies unset resource limits are
// filled from the effective policy at boot. Refs: FR-17.13, NFR-17.5
func TestProvision_PolicyDefaultsApplied(t *testing.T) {
	mgr := &fakeSandboxManager{}
	svc := newSvc(t, mgr, &fakeEventAppender{})
	_, err := svc.Register(context.Background(), regOpts("MGIT-1", "/work/a"))
	require.NoError(t, err)
	_, err = svc.EnsureRunning(context.Background(), "MGIT-1")
	require.NoError(t, err)

	def := model.DefaultSandboxPolicy()
	assert.Equal(t, def.CPUs, mgr.lastOpts.CPUs, "policy CPU default applied")
	assert.Equal(t, def.MemoryMB, mgr.lastOpts.MemoryMB)
}

// TestProvision_EnsureRunning_UnknownTask verifies an unregistered task
// fails closed.
func TestProvision_EnsureRunning_UnknownTask(t *testing.T) {
	svc := newSvc(t, &fakeSandboxManager{}, &fakeEventAppender{})
	_, err := svc.EnsureRunning(context.Background(), "MGIT-nope")
	assert.ErrorIs(t, err, model.ErrSandboxNotFound)
}

// TestProvision_BootFailure_StaysRetryable verifies a boot failure does
// not mark the sandbox booted (a later exec can retry) and emits no
// resumed event.
func TestProvision_BootFailure_StaysRetryable(t *testing.T) {
	mgr := &fakeSandboxManager{launchErr: errors.New("no /dev/kvm")}
	ev := &fakeEventAppender{}
	svc := newSvc(t, mgr, ev)
	_, err := svc.Register(context.Background(), regOpts("MGIT-1", "/work/a"))
	require.NoError(t, err)

	_, err = svc.EnsureRunning(context.Background(), "MGIT-1")
	require.Error(t, err)
	assert.Equal(t, []string{model.EventCreated}, ev.types(), "no resumed event on a failed boot")

	mgr.launchErr = nil
	_, err = svc.EnsureRunning(context.Background(), "MGIT-1")
	require.NoError(t, err, "a later exec retries the boot")
	assert.Equal(t, 2, mgr.launches)
}

// TestProvision_RegisterAuditFailure_NotRegistered verifies a failed
// registration audit aborts the registration (no unaudited sandbox).
func TestProvision_RegisterAuditFailure_NotRegistered(t *testing.T) {
	svc := newSvc(t, &fakeSandboxManager{}, &fakeEventAppender{failNth: 1})
	_, err := svc.Register(context.Background(), regOpts("MGIT-1", "/work/a"))
	require.Error(t, err)
	// The task is not bound, so it can be registered again once auditing works.
	_, err = svc.EnsureRunning(context.Background(), "MGIT-1")
	assert.ErrorIs(t, err, model.ErrSandboxNotFound)
}

// errPolicy fails to load the policy.
type errPolicy struct{}

func (errPolicy) Load(context.Context) (model.SandboxPolicy, error) {
	return model.SandboxPolicy{}, errors.New("policy unreadable")
}

// TestNewSandboxService_NilDeps covers every constructor guard.
func TestNewSandboxService_NilDeps(t *testing.T) {
	mgr := &fakeSandboxManager{}
	ev := &fakeEventAppender{}
	pol := fakePolicy{}
	clk := time.Now
	id := func() (string, error) { return "x", nil }
	for name, build := range map[string]func() (*SandboxService, error){
		"nil_manager": func() (*SandboxService, error) { return NewSandboxService(nil, ev, pol, clk, id) },
		"nil_events":  func() (*SandboxService, error) { return NewSandboxService(mgr, nil, pol, clk, id) },
		"nil_policy":  func() (*SandboxService, error) { return NewSandboxService(mgr, ev, nil, clk, id) },
		"nil_clock":   func() (*SandboxService, error) { return NewSandboxService(mgr, ev, pol, nil, id) },
		"nil_id":      func() (*SandboxService, error) { return NewSandboxService(mgr, ev, pol, clk, nil) },
	} {
		t.Run(name, func(t *testing.T) {
			_, err := build()
			assert.Error(t, err)
		})
	}
}

// TestRegister_InvalidOpts_Rejected covers the validation guard.
func TestRegister_InvalidOpts_Rejected(t *testing.T) {
	svc := newSvc(t, &fakeSandboxManager{}, &fakeEventAppender{})
	_, err := svc.Register(context.Background(), model.SandboxLaunchOptions{TaskID: ""}) // invalid
	assert.Error(t, err)
}

// TestRegister_IDGenFailure_Rejected covers the newID error path.
func TestRegister_IDGenFailure_Rejected(t *testing.T) {
	svc, err := NewSandboxService(&fakeSandboxManager{}, &fakeEventAppender{},
		fakePolicy{p: model.DefaultSandboxPolicy()}, func() time.Time { return time.Unix(0, 0).UTC() },
		func() (string, error) { return "", errors.New("entropy failure") })
	require.NoError(t, err)
	_, regErr := svc.Register(context.Background(), regOpts("MGIT-1", "/work/a"))
	assert.Error(t, regErr)
}

// TestEnsureRunning_PolicyLoadFailure covers the policy-load error path.
func TestEnsureRunning_PolicyLoadFailure(t *testing.T) {
	svc, err := NewSandboxService(&fakeSandboxManager{}, &fakeEventAppender{}, errPolicy{},
		func() time.Time { return time.Unix(0, 0).UTC() },
		func() (string, error) { return "01JXSB" + strings.Repeat("0", 21), nil })
	require.NoError(t, err)
	// Register uses no policy; EnsureRunning does.
	_, err = svc.Register(context.Background(), regOpts("MGIT-1", "/work/a"))
	require.NoError(t, err)
	_, err = svc.EnsureRunning(context.Background(), "MGIT-1")
	assert.Error(t, err)
}

// TestEnsureRunning_ResumeAuditFailure covers the boot-audit error path.
func TestEnsureRunning_ResumeAuditFailure(t *testing.T) {
	ev := &fakeEventAppender{failNth: 2} // created ok, resumed fails
	svc := newSvc(t, &fakeSandboxManager{}, ev)
	_, err := svc.Register(context.Background(), regOpts("MGIT-1", "/work/a"))
	require.NoError(t, err)
	_, err = svc.EnsureRunning(context.Background(), "MGIT-1")
	assert.Error(t, err)
}

// TestImageDigestOf covers the digest extraction including the no-@ edge.
func TestImageDigestOf(t *testing.T) {
	assert.Equal(t, "sha256:abc", imageDigestOf("img@sha256:abc"))
	assert.Equal(t, "", imageDigestOf("no-digest-ref"))
}
