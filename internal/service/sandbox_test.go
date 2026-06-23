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

// fakeSandboxManager records Launch/Exec calls and echoes opts into the info.
type fakeSandboxManager struct {
	launches  int
	lastOpts  model.SandboxLaunchOptions
	launchErr error

	execs              int
	lastExecID         string
	lastExecReq        model.ExecRequest
	execCtxHadDeadline bool
	execResult         *model.ExecResult
	execErr            error

	stops           int
	removes         int
	lastStopID      string
	lastRemoveID    string
	lastStopForce   bool
	lastRemoveForce bool
	stopErr         error
	removeErr       error

	listResult []model.SandboxInfo
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
		NetworkMode: opts.Network.Mode, NetworkAllowlist: opts.Network.Allowlist,
	}, nil
}
func (m *fakeSandboxManager) List(context.Context) ([]model.SandboxInfo, error) {
	return m.listResult, nil
}
func (m *fakeSandboxManager) Exec(ctx context.Context, id string, req model.ExecRequest) (*model.ExecResult, error) {
	m.execs++
	m.lastExecID, m.lastExecReq = id, req
	_, m.execCtxHadDeadline = ctx.Deadline()
	if m.execErr != nil {
		return nil, m.execErr
	}
	return m.execResult, nil
}
func (m *fakeSandboxManager) Stop(_ context.Context, id string, force bool) error {
	m.stops++
	m.lastStopID, m.lastStopForce = id, force
	return m.stopErr
}
func (m *fakeSandboxManager) Remove(_ context.Context, id string, force bool) error {
	m.removes++
	m.lastRemoveID, m.lastRemoveForce = id, force
	return m.removeErr
}
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

// TestNetOpen_NotDefault_RiskRecorded verifies that open-network mode is
// never the auto-selected default and that registering an open-mode
// sandbox records a risk note in the append-only created event (open NATs
// to the host network, disabling T3/T9). Refs: FR-17.7, MGIT-11.7.1
func TestNetOpen_NotDefault_RiskRecorded(t *testing.T) {
	// open is never auto-selected: the safe default posture is allowlist.
	assert.NotEqual(t, model.NetworkModeOpen, model.DefaultSandboxPolicy().Network.Mode,
		"open network must never be a default (T3 exfiltration / T9 lateral movement disabled)")

	mgr := &fakeSandboxManager{}
	ev := &fakeEventAppender{}
	svc := newSvc(t, mgr, ev)

	opts := regOpts("MGIT-11.7.1", "/work/open")
	opts.Network = model.NetworkPolicy{Mode: model.NetworkModeOpen}
	_, err := svc.Register(context.Background(), opts)
	require.NoError(t, err)

	require.Len(t, ev.events, 1, "registration appends exactly the created event")
	created := ev.events[0]
	assert.Equal(t, model.EventCreated, created.EventType)
	assert.Equal(t, model.NetworkModeOpen, created.NetworkMode, "the network mode is recorded")
	assert.Contains(t, created.Detail, "network_risk",
		"open mode emits a recorded risk note in the audit event")
	assert.Contains(t, created.Detail, "T3", "the risk note names the disabled defenses (T3/T9)")
}

// TestNetNonOpen_NoRiskNote verifies the host-confined modes (none,
// allowlist) record no risk note — the note is specific to open's
// user-accepted risk. Refs: FR-17.7, MGIT-11.7.1
func TestNetNonOpen_NoRiskNote(t *testing.T) {
	for _, mode := range []string{model.NetworkModeNone, model.NetworkModeAllowlist} {
		t.Run(mode, func(t *testing.T) {
			ev := &fakeEventAppender{}
			svc := newSvc(t, &fakeSandboxManager{}, ev)
			opts := regOpts("MGIT-11.7.1", "/work/"+mode)
			opts.Network = model.NetworkPolicy{Mode: mode}
			_, err := svc.Register(context.Background(), opts)
			require.NoError(t, err)
			require.Len(t, ev.events, 1)
			assert.Empty(t, ev.events[0].Detail, "%s is host-confined; no risk note", mode)
		})
	}
}

// fakeEgress records StartEgress/StopEgress calls.
type fakeEgress struct {
	started  []model.SandboxInfo
	stopped  []string
	startErr error
}

func (e *fakeEgress) StartEgress(_ context.Context, info model.SandboxInfo) error {
	if e.startErr != nil {
		return e.startErr
	}
	e.started = append(e.started, info)
	return nil
}
func (e *fakeEgress) StopEgress(sandboxID string) { e.stopped = append(e.stopped, sandboxID) }

// TestEgress_StartedOnBoot_StoppedOnRemove verifies the service drives the
// egress controller across the sandbox lifecycle, handing it the launched
// info (carrying the network mode + allowlist). Refs: FR-17.8, MGIT-11.7
func TestEgress_StartedOnBoot_StoppedOnRemove(t *testing.T) {
	mgr := &fakeSandboxManager{}
	eg := &fakeEgress{}
	svc := newSvc(t, mgr, &fakeEventAppender{})
	svc.SetEgressController(eg)

	opts := regOpts("MGIT-11.7", "/work/a")
	opts.Network = model.NetworkPolicy{Mode: model.NetworkModeAllowlist, Allowlist: []string{"registry.npmjs.org"}}
	reg, err := svc.Register(context.Background(), opts)
	require.NoError(t, err)
	assert.Empty(t, eg.started, "egress is not started until the VM boots")

	_, err = svc.EnsureRunning(context.Background(), "MGIT-11.7")
	require.NoError(t, err)
	require.Len(t, eg.started, 1, "egress starts on boot")
	assert.Equal(t, reg.ID, eg.started[0].ID)
	assert.Equal(t, model.NetworkModeAllowlist, eg.started[0].NetworkMode)

	require.NoError(t, svc.Remove(context.Background(), "MGIT-11.7", false))
	assert.Equal(t, []string{reg.ID}, eg.stopped, "egress stops on remove")
}

// TestEgress_StartFailure_RollsBackBoot verifies a failed egress start fails
// closed: the VM is rolled back and no resumed event is recorded. Refs: SEC-04
func TestEgress_StartFailure_RollsBackBoot(t *testing.T) {
	mgr := &fakeSandboxManager{}
	ev := &fakeEventAppender{}
	eg := &fakeEgress{startErr: errors.New("proxy bind failed")}
	svc := newSvc(t, mgr, ev)
	svc.SetEgressController(eg)

	_, err := svc.Register(context.Background(), regOpts("MGIT-11.7", "/work/a"))
	require.NoError(t, err)
	_, err = svc.EnsureRunning(context.Background(), "MGIT-11.7")
	require.Error(t, err, "a failed egress start fails the boot")
	assert.Equal(t, 1, mgr.removes, "the VM is rolled back")
	assert.Equal(t, []string{model.EventCreated}, ev.types(), "no resumed event when egress fails")
}

// fakePorts records StartPublish/StopPublish calls (SEC-09).
type fakePorts struct {
	started  []model.SandboxInfo
	lastPub  []model.PortPublish
	stopped  []string
	startErr error
}

func (p *fakePorts) StartPublish(_ context.Context, info model.SandboxInfo, ports []model.PortPublish) error {
	if p.startErr != nil {
		return p.startErr
	}
	p.started = append(p.started, info)
	p.lastPub = ports
	return nil
}
func (p *fakePorts) StopPublish(sandboxID string) { p.stopped = append(p.stopped, sandboxID) }

// TestPortPublish_StartedOnBoot_StoppedOnRemove verifies the service drives
// the port-publish controller across the lifecycle, handing it the launched
// info and the requested published ports. Refs: SEC-09, FR-17.8
func TestPortPublish_StartedOnBoot_StoppedOnRemove(t *testing.T) {
	mgr := &fakeSandboxManager{}
	pp := &fakePorts{}
	svc := newSvc(t, mgr, &fakeEventAppender{})
	svc.SetPortPublishController(pp)

	opts := regOpts("MGIT-11.10.12", "/work/a")
	opts.PublishPorts = []model.PortPublish{{HostPort: 8080, GuestPort: 3000}}
	reg, err := svc.Register(context.Background(), opts)
	require.NoError(t, err)
	assert.Empty(t, pp.started, "ports are not published until the VM boots")

	_, err = svc.EnsureRunning(context.Background(), "MGIT-11.10.12")
	require.NoError(t, err)
	require.Len(t, pp.started, 1, "ports are published on boot")
	assert.Equal(t, reg.ID, pp.started[0].ID)
	assert.Equal(t, []model.PortPublish{{HostPort: 8080, GuestPort: 3000}}, pp.lastPub)

	require.NoError(t, svc.Remove(context.Background(), "MGIT-11.10.12", false))
	assert.Equal(t, []string{reg.ID}, pp.stopped, "published ports are torn down on remove (no residue)")
}

// TestPortPublish_NoPortsRequested_NoStart verifies a sandbox that publishes
// nothing never calls the controller's StartPublish. Refs: SEC-09
func TestPortPublish_NoPortsRequested_NoStart(t *testing.T) {
	pp := &fakePorts{}
	svc := newSvc(t, &fakeSandboxManager{}, &fakeEventAppender{})
	svc.SetPortPublishController(pp)

	_, err := svc.Register(context.Background(), regOpts("MGIT-11.10.12", "/work/a"))
	require.NoError(t, err)
	_, err = svc.EnsureRunning(context.Background(), "MGIT-11.10.12")
	require.NoError(t, err)
	assert.Empty(t, pp.started, "no published ports means StartPublish is never called")
}

// TestPortPublish_StartFailure_RollsBackBoot verifies a failed port-publish
// bind fails closed: the VM is rolled back and no resumed event is recorded.
// Refs: SEC-09
func TestPortPublish_StartFailure_RollsBackBoot(t *testing.T) {
	mgr := &fakeSandboxManager{}
	ev := &fakeEventAppender{}
	pp := &fakePorts{startErr: errors.New("loopback bind failed")}
	svc := newSvc(t, mgr, ev)
	svc.SetPortPublishController(pp)

	opts := regOpts("MGIT-11.10.12", "/work/a")
	opts.PublishPorts = []model.PortPublish{{HostPort: 8080, GuestPort: 3000}}
	_, err := svc.Register(context.Background(), opts)
	require.NoError(t, err)
	_, err = svc.EnsureRunning(context.Background(), "MGIT-11.10.12")
	require.Error(t, err, "a failed port-publish start fails the boot")
	assert.Equal(t, 1, mgr.removes, "the VM is rolled back")
	assert.Equal(t, []string{model.EventCreated}, ev.types(), "no resumed event when port publish fails")
}

// TestPortPublish_StoppedOnTeardownPaths verifies every teardown path (land,
// idle-suspend) releases the published ports, so no host listener outlives
// the sandbox. Refs: SEC-09, FR-17.19, NFR-17.3
func TestPortPublish_StoppedOnTeardownPaths(t *testing.T) {
	t.Run("suspend_idle_stops_publish", func(t *testing.T) {
		mgr := &fakeSandboxManager{}
		pp := &fakePorts{}
		svc := newSvc(t, mgr, &fakeEventAppender{})
		svc.SetPortPublishController(pp)
		opts := regOpts("MGIT-11.10.12", "/work/a")
		opts.PublishPorts = []model.PortPublish{{HostPort: 8080, GuestPort: 3000}}
		reg, err := svc.Register(context.Background(), opts)
		require.NoError(t, err)
		_, err = svc.EnsureRunning(context.Background(), "MGIT-11.10.12")
		require.NoError(t, err)

		suspended, err := svc.SuspendIdle(context.Background(), 0)
		require.NoError(t, err)
		require.Equal(t, []string{"MGIT-11.10.12"}, suspended)
		assert.Equal(t, []string{reg.ID}, pp.stopped, "suspend releases the published ports")
	})
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

// TestEnsureRunning_ResumeAuditFailure covers the boot-audit error path and
// verifies the just-booted VM is rolled back (Stop+Remove) so no
// running-but-unaudited sandbox is ever left behind, and the registration
// stays retryable. Refs: MGIT-11.10.8 (security audit: partial-failure)
func TestEnsureRunning_ResumeAuditFailure(t *testing.T) {
	mgr := &fakeSandboxManager{}
	ev := &fakeEventAppender{failNth: 2} // created ok, resumed fails
	svc := newSvc(t, mgr, ev)
	reg, err := svc.Register(context.Background(), regOpts("MGIT-1", "/work/a"))
	require.NoError(t, err)

	_, err = svc.EnsureRunning(context.Background(), "MGIT-1")
	require.Error(t, err)
	assert.Equal(t, 1, mgr.stops, "the booted-but-unaudited VM is stopped")
	assert.Equal(t, 1, mgr.removes, "the booted-but-unaudited VM is removed")
	assert.Equal(t, reg.ID, mgr.lastRemoveID, "exactly the just-booted VM is rolled back")

	// The registration is left un-booted and retryable.
	info, err := svc.Status(context.Background(), "MGIT-1")
	require.NoError(t, err)
	assert.Equal(t, model.StateCreated, info.State, "rollback leaves the sandbox un-booted")
}

// TestList_ReturnsRegisteredSandboxes verifies List returns every
// registered sandbox (created + running), in a stable task-ID order.
func TestList_ReturnsRegisteredSandboxes(t *testing.T) {
	svc := newSvc(t, &fakeSandboxManager{}, &fakeEventAppender{})
	_, err := svc.Register(context.Background(), regOpts("MGIT-2", "/work/b"))
	require.NoError(t, err)
	_, err = svc.Register(context.Background(), regOpts("MGIT-1", "/work/a"))
	require.NoError(t, err)

	list, err := svc.List(context.Background())
	require.NoError(t, err)
	require.Len(t, list, 2)
	assert.Equal(t, "MGIT-1", list[0].TaskID, "list is sorted by task ID")
	assert.Equal(t, "MGIT-2", list[1].TaskID)
}

// TestStatus_KnownAndUnknown covers the status lookup for a registered and
// an unregistered task.
func TestStatus_KnownAndUnknown(t *testing.T) {
	svc := newSvc(t, &fakeSandboxManager{}, &fakeEventAppender{})
	_, err := svc.Register(context.Background(), regOpts("MGIT-1", "/work/a"))
	require.NoError(t, err)

	info, err := svc.Status(context.Background(), "MGIT-1")
	require.NoError(t, err)
	assert.Equal(t, "MGIT-1", info.TaskID)

	_, err = svc.Status(context.Background(), "MGIT-nope")
	assert.ErrorIs(t, err, model.ErrSandboxNotFound)
}

// TestRemove_BootedSandbox_TearsDownAndAudits verifies removing a booted
// sandbox stops + removes the VM, audits a destroyed event, and frees the
// task binding (so the task can be re-registered).
func TestRemove_BootedSandbox_TearsDownAndAudits(t *testing.T) {
	mgr := &fakeSandboxManager{}
	ev := &fakeEventAppender{}
	svc := newSvc(t, mgr, ev)
	reg, err := svc.Register(context.Background(), regOpts("MGIT-1", "/work/a"))
	require.NoError(t, err)
	_, err = svc.EnsureRunning(context.Background(), "MGIT-1")
	require.NoError(t, err)

	require.NoError(t, svc.Remove(context.Background(), "MGIT-1", true))
	assert.Equal(t, 1, mgr.stops, "the booted VM is stopped")
	assert.Equal(t, reg.ID, mgr.lastRemoveID, "the booted VM is removed")
	assert.True(t, mgr.lastRemoveForce, "force is forwarded to the backend")
	assert.Equal(t, []string{model.EventCreated, model.EventResumed, model.EventDestroyed}, ev.types())

	// The task binding is freed: re-registration succeeds, and status is gone.
	_, err = svc.Status(context.Background(), "MGIT-1")
	assert.ErrorIs(t, err, model.ErrSandboxNotFound)
	_, err = svc.Register(context.Background(), regOpts("MGIT-1", "/work/a"))
	assert.NoError(t, err, "the task can be re-registered after removal")
}

// TestRemove_UnbootedSandbox_NoBackendCall verifies removing a registered
// but never-booted sandbox audits destroyed without touching the backend.
func TestRemove_UnbootedSandbox_NoBackendCall(t *testing.T) {
	mgr := &fakeSandboxManager{}
	ev := &fakeEventAppender{}
	svc := newSvc(t, mgr, ev)
	_, err := svc.Register(context.Background(), regOpts("MGIT-1", "/work/a"))
	require.NoError(t, err)

	require.NoError(t, svc.Remove(context.Background(), "MGIT-1", false))
	assert.Zero(t, mgr.stops, "an un-booted sandbox has no VM to stop")
	assert.Zero(t, mgr.removes)
	assert.Equal(t, []string{model.EventCreated, model.EventDestroyed}, ev.types())
}

// TestRemove_BackendRemoveError_Surfaces verifies a backend remove failure
// (after a successful stop) surfaces and leaves the sandbox registered.
func TestRemove_BackendRemoveError_Surfaces(t *testing.T) {
	mgr := &fakeSandboxManager{removeErr: errors.New("vmm remove failed")}
	svc := newSvc(t, mgr, &fakeEventAppender{})
	_, err := svc.Register(context.Background(), regOpts("MGIT-1", "/work/a"))
	require.NoError(t, err)
	_, err = svc.EnsureRunning(context.Background(), "MGIT-1")
	require.NoError(t, err)

	require.Error(t, svc.Remove(context.Background(), "MGIT-1", true))
	assert.Equal(t, 1, mgr.stops, "stop ran before the failing remove")
	_, err = svc.Status(context.Background(), "MGIT-1")
	assert.NoError(t, err, "a failed remove leaves the sandbox registered for retry")
}

// TestRemove_AuditFailure_Surfaces verifies a destroyed-audit failure
// surfaces (the VM is already torn down; the registration is retryable).
func TestRemove_AuditFailure_Surfaces(t *testing.T) {
	mgr := &fakeSandboxManager{}
	ev := &fakeEventAppender{failNth: 2} // created ok, destroyed fails (unbooted: only 2 events)
	svc := newSvc(t, mgr, ev)
	_, err := svc.Register(context.Background(), regOpts("MGIT-1", "/work/a"))
	require.NoError(t, err)
	require.Error(t, svc.Remove(context.Background(), "MGIT-1", false))
}

// TestRemove_UnknownTask_NotFound verifies removing an unregistered task
// fails closed.
func TestRemove_UnknownTask_NotFound(t *testing.T) {
	svc := newSvc(t, &fakeSandboxManager{}, &fakeEventAppender{})
	assert.ErrorIs(t, svc.Remove(context.Background(), "MGIT-nope", false), model.ErrSandboxNotFound)
}

// TestRemove_BackendStopError_Surfaces verifies a backend teardown failure
// surfaces and the sandbox stays registered (retryable).
func TestRemove_BackendStopError_Surfaces(t *testing.T) {
	mgr := &fakeSandboxManager{stopErr: errors.New("vmm hung")}
	svc := newSvc(t, mgr, &fakeEventAppender{})
	_, err := svc.Register(context.Background(), regOpts("MGIT-1", "/work/a"))
	require.NoError(t, err)
	_, err = svc.EnsureRunning(context.Background(), "MGIT-1")
	require.NoError(t, err)

	require.Error(t, svc.Remove(context.Background(), "MGIT-1", false))
	_, err = svc.Status(context.Background(), "MGIT-1")
	assert.NoError(t, err, "a failed teardown leaves the sandbox registered for retry")
}

// TestExec_BootsThenRoutesToManager verifies Exec lazily boots the
// sandbox (EnsureRunning) and routes the command to the manager with the
// booted sandbox ID, returning its result. Refs: FR-17.9, FR-17.11
func TestExec_BootsThenRoutesToManager(t *testing.T) {
	mgr := &fakeSandboxManager{execResult: &model.ExecResult{Stdout: []byte("hi\n"), ExitCode: 0}}
	svc := newSvc(t, mgr, &fakeEventAppender{})
	reg, err := svc.Register(context.Background(), regOpts("MGIT-9.1", "/work/a"))
	require.NoError(t, err)

	res, err := svc.Exec(context.Background(), "MGIT-9.1", model.ExecRequest{Command: []string{"sh", "-c", "echo hi"}})
	require.NoError(t, err)
	assert.Equal(t, 1, mgr.launches, "exec boots the sandbox on first use")
	assert.Equal(t, reg.ID, mgr.lastExecID, "exec routes to the booted sandbox ID")
	assert.Equal(t, []string{"sh", "-c", "echo hi"}, mgr.lastExecReq.Command)
	assert.Equal(t, "hi\n", string(res.Stdout))
}

// TestExec_PositiveTimeout_BoundsCommand verifies a positive per-exec
// timeout reaches the manager as a context deadline (so both backends
// enforce it), while a zero timeout leaves the context unbounded.
// Refs: FR-17.11
func TestExec_PositiveTimeout_BoundsCommand(t *testing.T) {
	t.Run("positive_timeout_sets_deadline", func(t *testing.T) {
		mgr := &fakeSandboxManager{execResult: &model.ExecResult{}}
		svc := newSvc(t, mgr, &fakeEventAppender{})
		_, err := svc.Register(context.Background(), regOpts("MGIT-1", "/work/a"))
		require.NoError(t, err)
		_, err = svc.Exec(context.Background(), "MGIT-1",
			model.ExecRequest{Command: []string{"true"}, Timeout: 5 * time.Second})
		require.NoError(t, err)
		assert.True(t, mgr.execCtxHadDeadline, "a positive timeout bounds the exec context")
	})
	t.Run("zero_timeout_no_deadline", func(t *testing.T) {
		mgr := &fakeSandboxManager{execResult: &model.ExecResult{}}
		svc := newSvc(t, mgr, &fakeEventAppender{})
		_, err := svc.Register(context.Background(), regOpts("MGIT-1", "/work/a"))
		require.NoError(t, err)
		_, err = svc.Exec(context.Background(), "MGIT-1", model.ExecRequest{Command: []string{"true"}})
		require.NoError(t, err)
		assert.False(t, mgr.execCtxHadDeadline, "a zero timeout leaves the context unbounded")
	})
}

// TestExec_UnknownTask_Rejected verifies exec on an unregistered task
// fails closed (no boot, no route).
func TestExec_UnknownTask_Rejected(t *testing.T) {
	mgr := &fakeSandboxManager{}
	svc := newSvc(t, mgr, &fakeEventAppender{})
	_, err := svc.Exec(context.Background(), "MGIT-nope", model.ExecRequest{Command: []string{"true"}})
	assert.ErrorIs(t, err, model.ErrSandboxNotFound)
	assert.Zero(t, mgr.execs, "no routing for an unregistered task")
}

// TestExec_ManagerError_Surfaces verifies a transport/manager error
// surfaces from Exec.
func TestExec_ManagerError_Surfaces(t *testing.T) {
	mgr := &fakeSandboxManager{execErr: model.ErrSandboxBackendUnavailable}
	svc := newSvc(t, mgr, &fakeEventAppender{})
	_, err := svc.Register(context.Background(), regOpts("MGIT-1", "/work/a"))
	require.NoError(t, err)
	_, err = svc.Exec(context.Background(), "MGIT-1", model.ExecRequest{Command: []string{"true"}})
	assert.ErrorIs(t, err, model.ErrSandboxBackendUnavailable)
}

// TestImageDigestOf covers the digest extraction including the no-@ edge.
func TestImageDigestOf(t *testing.T) {
	assert.Equal(t, "sha256:abc", imageDigestOf("img@sha256:abc"))
	assert.Equal(t, "", imageDigestOf("no-digest-ref"))
}
