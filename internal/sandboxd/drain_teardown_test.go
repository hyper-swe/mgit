package sandboxd

import (
	"context"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/hyper-swe/mgit/internal/model"
)

// drainRecorder is a SandboxDispatcher that records the teardown calls drain
// makes through the service layer. Refs: MGIT-107
type drainRecorder struct {
	mu        sync.Mutex
	tasks     []model.SandboxInfo
	removed   []string
	forced    []bool
	failTasks map[string]error
	listErr   error
}

func newDrainRecorder(taskIDs ...string) *drainRecorder {
	r := &drainRecorder{failTasks: map[string]error{}}
	for _, id := range taskIDs {
		r.tasks = append(r.tasks, model.SandboxInfo{ID: "sbx-" + id, TaskID: id, State: model.StateRunning})
	}
	return r
}

func (r *drainRecorder) Register(context.Context, model.SandboxLaunchOptions) (*model.SandboxInfo, error) {
	return nil, assert.AnError
}
func (r *drainRecorder) Exec(context.Context, string, model.ExecRequest) (*model.ExecResult, error) {
	return &model.ExecResult{}, nil
}
func (r *drainRecorder) List(context.Context) ([]model.SandboxInfo, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.listErr != nil {
		return nil, r.listErr
	}
	out := make([]model.SandboxInfo, len(r.tasks))
	copy(out, r.tasks)
	return out, nil
}
func (r *drainRecorder) Remove(_ context.Context, taskID string, force bool) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if err, bad := r.failTasks[taskID]; bad {
		return err
	}
	r.removed = append(r.removed, taskID)
	r.forced = append(r.forced, force)
	return nil
}
func (r *drainRecorder) Status(context.Context, string) (*model.SandboxInfo, error) {
	return nil, model.ErrSandboxNotFound
}
func (r *drainRecorder) SyncWorktree(context.Context, string, model.WorktreeSyncOptions) (*model.WorktreeSyncReport, error) {
	return nil, assert.AnError
}

func (r *drainRecorder) snapshot() ([]string, []bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]string(nil), r.removed...), append([]bool(nil), r.forced...)
}

// THE DEFECT. drain reached the BACKEND manager directly, while the terminal
// `destroyed` event is written one layer up by the service. So an orderly
// shutdown — SIGTERM, SIGINT, and the daemon's own idle exit, which is the
// COMMON case — left no terminal event at all, and the next daemon then found
// a registration it could not verify and stamped it `killed / unsupervised`.
//
// Reproduced live on macOS/libkrun before this fix: a real sandbox, a real
// SIGTERM, and the trail read
//
//	DRAIN-1 | created
//	DRAIN-1 | resumed
//	DRAIN-1 | killed | {"reason":"unsupervised: ..."}
//
// which is the record a daemon CRASH is supposed to produce. Routing drain
// through the service means the event is written by the one place that owns
// teardown semantics — not copied into a second writer, which is how the two
// paths drift apart again. Refs: MGIT-107, MGIT-102, FR-17.19
func TestDaemonDrain_RoutesThroughTheServiceThatWritesTheTerminalEvent(t *testing.T) {
	manager := newFakeManager("01JXSB1", "01JXSB2")
	svc := newDrainRecorder("TASK-A", "TASK-B")
	cfg, _ := testConfig(t, manager)
	cfg.Service = svc

	d, err := New(cfg)
	require.NoError(t, err)
	require.NoError(t, d.drain(context.Background()))

	removed, forced := svc.snapshot()
	assert.ElementsMatch(t, []string{"TASK-A", "TASK-B"}, removed,
		"every sandbox is torn down through the service, which is what writes `destroyed`")
	for _, f := range forced {
		assert.True(t, f, "a shutdown drain forces teardown; it cannot wait on a graceful guest")
	}

	// And it must NOT also reach the backend directly: a second teardown path
	// is what let the two records diverge in the first place.
	assert.Empty(t, manager.stopped, "drain must not bypass the service to the backend manager")
	assert.Empty(t, manager.removed, "drain must not bypass the service to the backend manager")
}

// One sandbox that cannot be torn down must not strand the others, and must
// not be recorded as cleanly destroyed either — the service refuses to write
// the terminal event when the stop fails, so the honest `killed/unsupervised`
// record from the next daemon is the right outcome for that one.
// Refs: MGIT-107
func TestDaemonDrain_OneStuckSandbox_DoesNotStrandTheRest(t *testing.T) {
	manager := newFakeManager()
	svc := newDrainRecorder("TASK-A", "TASK-STUCK", "TASK-C")
	svc.failTasks["TASK-STUCK"] = assert.AnError
	cfg, logs := testConfig(t, manager)
	cfg.Service = svc

	d, err := New(cfg)
	require.NoError(t, err)
	require.NoError(t, d.drain(context.Background()), "a stuck sandbox does not abort the shutdown")

	removed, _ := svc.snapshot()
	assert.ElementsMatch(t, []string{"TASK-A", "TASK-C"}, removed,
		"the other sandboxes are still torn down")
	assert.NotContains(t, removed, "TASK-STUCK")
	assert.Contains(t, logs.String(), `"drain_error"`, "the failure is logged, not swallowed")
}

// A build with no wired service (backend-only daemon) still has to drain, or
// shutdown would leave VMs running. It falls back to the backend manager and
// says so, rather than silently doing nothing. Refs: MGIT-107, MGIT-11.10.8
func TestDaemonDrain_WithoutAService_FallsBackToTheBackendManager(t *testing.T) {
	manager := newFakeManager("01JXSB1", "01JXSB2")
	cfg, _ := testConfig(t, manager)
	cfg.Service = nil

	d, err := New(cfg)
	require.NoError(t, err)
	require.NoError(t, d.drain(context.Background()))

	assert.ElementsMatch(t, []string{"01JXSB1", "01JXSB2"}, manager.stopped)
	assert.ElementsMatch(t, []string{"01JXSB1", "01JXSB2"}, manager.removed)
}

// A List failure still surfaces, so a shutdown that could not even enumerate
// what it was supposed to drain does not report itself clean. Refs: MGIT-107
func TestDaemonDrain_ServiceListFailure_IsSurfaced(t *testing.T) {
	svc := newDrainRecorder()
	svc.listErr = assert.AnError
	cfg, _ := testConfig(t, newFakeManager())
	cfg.Service = svc

	d, err := New(cfg)
	require.NoError(t, err)
	require.Error(t, d.drain(context.Background()))
}

// panicOnTeardown panics for one task, to prove a drain survives it.
type panicOnTeardown struct {
	*drainRecorder
	panicTask string
	panicList bool
}

func (p *panicOnTeardown) List(ctx context.Context) ([]model.SandboxInfo, error) {
	if p.panicList {
		panic("induced panic in drain list")
	}
	return p.drainRecorder.List(ctx)
}

func (p *panicOnTeardown) Remove(ctx context.Context, taskID string, force bool) error {
	if taskID == p.panicTask {
		panic("induced panic tearing down " + taskID)
	}
	return p.drainRecorder.Remove(ctx, taskID, force)
}

// A panic during shutdown teardown must not take the daemon down with it.
//
// The request-handling path already recovers panics per connection; the drain
// path did not, so one bad teardown killed the process mid-shutdown and every
// sandbox after it in the list was left RUNNING — orphaned VMs, which is the
// failure the lifeline and Pdeathsig work exists to prevent. Found by an
// existing panic-injection test failing once drain started calling the service.
// Refs: MGIT-107, FR-17.19
func TestDaemonDrain_PanicTearingDownOneSandbox_StillDrainsTheRest(t *testing.T) {
	svc := &panicOnTeardown{drainRecorder: newDrainRecorder("TASK-A", "TASK-BOOM", "TASK-C"), panicTask: "TASK-BOOM"}
	cfg, logs := testConfig(t, newFakeManager())
	cfg.Service = svc

	d, err := New(cfg)
	require.NoError(t, err)
	require.NotPanics(t, func() {
		require.NoError(t, d.drain(context.Background()))
	}, "a panicking teardown must not abort the shutdown")

	removed, _ := svc.snapshot()
	assert.ElementsMatch(t, []string{"TASK-A", "TASK-C"}, removed,
		"the sandboxes after the panicking one are still torn down, not left running")
	assert.Contains(t, logs.String(), `"drain_error"`)
}

// The same protection around the enumeration itself. Refs: MGIT-107
func TestDaemonDrain_PanicListingSandboxes_IsReportedNotFatal(t *testing.T) {
	svc := &panicOnTeardown{drainRecorder: newDrainRecorder("TASK-A"), panicList: true}
	cfg, _ := testConfig(t, newFakeManager())
	cfg.Service = svc

	d, err := New(cfg)
	require.NoError(t, err)
	var drainErr error
	require.NotPanics(t, func() { drainErr = d.drain(context.Background()) })
	require.Error(t, drainErr, "a drain that could not enumerate must not report itself clean")
}
