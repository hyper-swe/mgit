package service

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/hyper-swe/mgit/internal/model"
)

// gatedManager is a SandboxManager whose Launch BLOCKS for one nominated task
// until the test releases it — a VM boot the test controls the duration of.
//
// It stands in for the thing this file is about: a real boot takes 16-59 s at
// the fleet widths docs/E2E-MATRIX.md measures, and for as long as the service
// held its mutex across one, every other sandbox's operation queued behind it
// and died on the client's socket deadline (MGIT-122). A test that waited on a
// real boot would be measuring the host; blocking on a channel measures the
// lock boundary, which is the property under test.
type gatedManager struct {
	mu       sync.Mutex
	launches map[string]int
	stops    int
	removes  int
	execs    int

	blockTask string        // Launch blocks only for this task
	release   chan struct{} // closed by the test to let the blocked Launch return
	entered   chan string   // one send per Launch entry (buffered)
	launchErr error         // returned by the BLOCKED launch, after release
}

func newGatedManager(blockTask string) *gatedManager {
	return &gatedManager{
		launches:  map[string]int{},
		blockTask: blockTask,
		release:   make(chan struct{}),
		entered:   make(chan string, 16),
	}
}

func (m *gatedManager) Launch(ctx context.Context, opts model.SandboxLaunchOptions) (*model.SandboxInfo, error) {
	m.mu.Lock()
	m.launches[opts.TaskID]++
	m.mu.Unlock()
	select {
	case m.entered <- opts.TaskID:
	default:
	}
	if opts.TaskID == m.blockTask {
		select {
		case <-m.release:
			if m.launchErr != nil {
				return nil, m.launchErr
			}
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	return &model.SandboxInfo{
		ID: opts.SandboxID, TaskID: opts.TaskID, WorktreePath: opts.WorktreePath,
		Backend: model.BackendKVM, State: model.StateRunning, MemoryMB: opts.MemoryMB,
	}, nil
}

func (m *gatedManager) List(context.Context) ([]model.SandboxInfo, error) { return nil, nil }

func (m *gatedManager) Exec(_ context.Context, id string, _ model.ExecRequest) (*model.ExecResult, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.execs++
	return &model.ExecResult{ExitCode: 0, Stdout: []byte(id)}, nil
}

func (m *gatedManager) Stop(_ context.Context, _ string, _ bool) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.stops++
	return nil
}

func (m *gatedManager) Remove(_ context.Context, _ string, _ bool) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.removes++
	return nil
}

func (m *gatedManager) Resolve(context.Context, string) (*model.SandboxInfo, error) {
	return nil, nil
}

func (m *gatedManager) launchCount(taskID string) int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.launches[taskID]
}

func (m *gatedManager) teardowns() (stops, removes int) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.stops, m.removes
}

// awaitLaunch blocks until the manager reports the named task entered Launch.
func (m *gatedManager) awaitLaunch(t *testing.T, taskID string) {
	t.Helper()
	deadline := time.After(3 * time.Second)
	for {
		select {
		case got := <-m.entered:
			if got == taskID {
				return
			}
		case <-deadline:
			t.Fatalf("task %q never entered Launch", taskID)
		}
	}
}

// completes reports whether fn returned within d.
func completes(fn func(), d time.Duration) bool {
	done := make(chan struct{})
	go func() { defer close(done); fn() }()
	select {
	case <-done:
		return true
	case <-time.After(d):
		return false
	}
}

// TestEnsureRunning_WhileAnotherSandboxBootsExecOnHealthySandboxCompletes is
// the regression this ticket exists for: one VM boot must not block every
// other operation on the daemon.
//
// The service mutex used to be held across manager.Launch, so an exec against
// a DIFFERENT, already-healthy sandbox waited out the whole boot and died on
// the client's 30 s socket deadline — rendered to the agent as "the guest
// stopped answering", about a guest that was fine. Refs: MGIT-122, NFR-17.6
func TestEnsureRunning_WhileAnotherSandboxBoots_ExecOnHealthySandboxCompletes(t *testing.T) {
	mgr := newGatedManager("MGIT-122.B")
	svc := newSvc(t, mgr, &fakeEventAppender{})
	ctx := context.Background()
	require.NoError(t, registerAndBoot(ctx, svc, "MGIT-122.A", "/wt/a"))
	_, err := svc.Register(ctx, regOpts("MGIT-122.B", "/wt/b"))
	require.NoError(t, err)

	booting := make(chan struct{})
	go func() {
		defer close(booting)
		_, _ = svc.EnsureRunning(ctx, "MGIT-122.B")
	}()
	mgr.awaitLaunch(t, "MGIT-122.B")

	var execErr error
	ok := completes(func() {
		_, execErr = svc.Exec(ctx, "MGIT-122.A", model.ExecRequest{Command: []string{"/bin/echo"}})
	}, 2*time.Second)
	require.True(t, ok, "an exec against healthy sandbox A did not complete while B was booting: "+
		"the service serialized it behind the boot, which is exactly what times the client out")
	require.NoError(t, execErr)

	close(mgr.release)
	<-booting
}

// TestEnsureRunning_ConcurrentCallsForOneTask_LaunchOneVM proves narrowing the
// lock did not open the race it could have: N callers arriving for the SAME
// unbooted task must converge on ONE boot, not start one VM each. A second VM
// for one task would be an unaccounted guest holding memory no registration
// describes. Refs: MGIT-122, FR-17.9
func TestEnsureRunning_ConcurrentCallsForOneTask_LaunchOneVM(t *testing.T) {
	mgr := newGatedManager("MGIT-122.B")
	svc := newSvc(t, mgr, &fakeEventAppender{})
	ctx := context.Background()
	_, err := svc.Register(ctx, regOpts("MGIT-122.B", "/wt/b"))
	require.NoError(t, err)

	const callers = 5
	infos := make([]*model.SandboxInfo, callers)
	errs := make([]error, callers)
	var wg sync.WaitGroup
	wg.Add(callers)
	for i := range callers {
		go func() {
			defer wg.Done()
			infos[i], errs[i] = svc.EnsureRunning(ctx, "MGIT-122.B")
		}()
	}
	mgr.awaitLaunch(t, "MGIT-122.B")
	// Give the other callers time to arrive and find the boot in flight.
	time.Sleep(100 * time.Millisecond)
	close(mgr.release)
	wg.Wait()

	assert.Equal(t, 1, mgr.launchCount("MGIT-122.B"), "concurrent EnsureRunning booted more than one VM for one task")
	for i := range callers {
		require.NoError(t, errs[i])
		require.NotNil(t, infos[i])
		assert.Equal(t, infos[0].ID, infos[i].ID, "callers disagree about which sandbox they got")
	}
	stops, removes := mgr.teardowns()
	assert.Zero(t, stops+removes, "a successful boot tore something down")
}

// TestEnsureRunning_ConcurrentCallsForOneTask_ShareTheBootFailure verifies the
// losers of the boot race receive the winner's failure rather than each
// retrying a launch against a backend that has just failed. One failure, one
// attempt: a boot storm against a broken backend is not a recovery strategy.
// Refs: MGIT-122
func TestEnsureRunning_ConcurrentCallsForOneTask_ShareTheBootFailure(t *testing.T) {
	mgr := newGatedManager("MGIT-122.B")
	mgr.launchErr = errors.New("backend refused")
	svc := newSvc(t, mgr, &fakeEventAppender{})
	ctx := context.Background()
	_, err := svc.Register(ctx, regOpts("MGIT-122.B", "/wt/b"))
	require.NoError(t, err)

	const callers = 4
	errs := make([]error, callers)
	var wg sync.WaitGroup
	wg.Add(callers)
	for i := range callers {
		go func() {
			defer wg.Done()
			_, errs[i] = svc.EnsureRunning(ctx, "MGIT-122.B")
		}()
	}
	mgr.awaitLaunch(t, "MGIT-122.B")
	time.Sleep(100 * time.Millisecond)
	close(mgr.release)
	wg.Wait()

	assert.Equal(t, 1, mgr.launchCount("MGIT-122.B"), "a failed boot was retried by every waiter")
	for i := range callers {
		require.Error(t, errs[i], "caller %d was told a failed boot succeeded", i)
		assert.Contains(t, errs[i].Error(), "backend refused")
	}
	// The registration stays and is retryable: a later call boots again.
	mgr.launchErr = nil
	mgr.blockTask = ""
	info, err := svc.EnsureRunning(ctx, "MGIT-122.B")
	require.NoError(t, err)
	assert.Equal(t, model.StateRunning, info.State)
}

// TestRemove_DuringInFlightBoot_WaitsAndTearsDownTheVM closes the window
// narrowing the lock would otherwise open: a remove arriving mid-boot must not
// drop the registration and leave the VM the boot is about to produce running
// with nothing tracking it.
//
// The remove waits for THAT task's boot to settle and then tears the VM down;
// the audit trail stays coherent (created -> resumed -> destroyed), which is
// the soak's invariant I5. Refs: MGIT-122, FR-17.9, FR-17.18, MGIT-103
func TestRemove_DuringInFlightBoot_WaitsAndTearsDownTheVM(t *testing.T) {
	mgr := newGatedManager("MGIT-122.B")
	ev := &fakeEventAppender{}
	svc := newSvc(t, mgr, ev)
	ctx := context.Background()
	_, err := svc.Register(ctx, regOpts("MGIT-122.B", "/wt/b"))
	require.NoError(t, err)

	booting := make(chan struct{})
	go func() {
		defer close(booting)
		_, _ = svc.EnsureRunning(ctx, "MGIT-122.B")
	}()
	mgr.awaitLaunch(t, "MGIT-122.B")

	removed := make(chan error, 1)
	go func() { removed <- svc.Remove(ctx, "MGIT-122.B", true) }()
	select {
	case err := <-removed:
		t.Fatalf("remove completed while B's boot was still in flight (err=%v): "+
			"it would have dropped the registration and stranded the VM the boot produces", err)
	case <-time.After(200 * time.Millisecond):
	}

	close(mgr.release)
	<-booting
	select {
	case err := <-removed:
		require.NoError(t, err)
	case <-time.After(3 * time.Second):
		t.Fatal("remove never completed after the boot settled")
	}

	stops, removes := mgr.teardowns()
	assert.Equal(t, 1, stops, "the booted VM was not stopped")
	assert.Equal(t, 1, removes, "the booted VM was not removed")
	assert.Equal(t, []string{"created", "resumed", "destroyed"}, ev.types(),
		"the audit trail must reconstruct one coherent life, terminal event last")
	_, err = svc.Status(ctx, "MGIT-122.B")
	assert.ErrorIs(t, err, model.ErrSandboxNotFound)
}

// TestReapExpired_DuringInFlightBoot_SkipsUntilItSettles verifies the TTL
// sweep does the same: a registration whose VM is mid-boot is not reaped out
// from under the boot (which would delete the registration and leave the VM
// unaccounted). An in-flight boot IS activity; the next sweep reaps it if it
// is still expired. Refs: MGIT-122, FR-17.9
func TestReapExpired_DuringInFlightBoot_SkipsUntilItSettles(t *testing.T) {
	mgr := newGatedManager("MGIT-122.B")
	ev := &fakeEventAppender{}
	now := time.Unix(0, 0).UTC()
	var clockMu sync.Mutex
	svc, err := NewSandboxService(mgr, ev, fakePolicy{p: model.DefaultSandboxPolicy()},
		func() time.Time { clockMu.Lock(); defer clockMu.Unlock(); return now },
		func() (string, error) { return "01JXSB000000000000000000B", nil })
	require.NoError(t, err)
	ctx := context.Background()
	opts := regOpts("MGIT-122.B", "/wt/b")
	opts.TTL = time.Minute
	_, err = svc.Register(ctx, opts)
	require.NoError(t, err)

	booting := make(chan struct{})
	go func() {
		defer close(booting)
		_, _ = svc.EnsureRunning(ctx, "MGIT-122.B")
	}()
	mgr.awaitLaunch(t, "MGIT-122.B")

	clockMu.Lock()
	now = now.Add(2 * time.Minute) // well past the TTL
	clockMu.Unlock()
	reaped, err := svc.ReapExpired(ctx)
	require.NoError(t, err)
	assert.Empty(t, reaped, "the sweep reaped a sandbox whose VM was still booting")
	stops, removes := mgr.teardowns()
	assert.Zero(t, stops+removes, "the sweep tore down a VM that was still being created")

	close(mgr.release)
	<-booting
	_, err = svc.Status(ctx, "MGIT-122.B")
	require.NoError(t, err, "the registration survived its own boot")
}

// TestStagePendingEgressPolicy_DuringInFlightBoot_RefusesAsBooted keeps
// MGIT-109's guarantee intact across the narrower lock.
//
// Staging a policy onto an unbooted sandbox works because the launch reads
// reg.opts. Once a boot is IN FLIGHT the launch has already taken its options,
// so a stage that reported success would be a silent fail-open: the caller is
// told an allowlist is in force while the VM boots with the previous one. The
// boot window is therefore refused exactly like a booted sandbox — the caller
// re-routes to the live enforcer. Refs: MGIT-122, MGIT-109, SEC-04
func TestStagePendingEgressPolicy_DuringInFlightBoot_RefusesAsBooted(t *testing.T) {
	mgr := newGatedManager("MGIT-122.B")
	svc := newSvc(t, mgr, &fakeEventAppender{})
	ctx := context.Background()
	info, err := svc.Register(ctx, regOpts("MGIT-122.B", "/wt/b"))
	require.NoError(t, err)

	booting := make(chan struct{})
	go func() {
		defer close(booting)
		_, _ = svc.EnsureRunning(ctx, "MGIT-122.B")
	}()
	mgr.awaitLaunch(t, "MGIT-122.B")

	_, err = svc.StagePendingEgressPolicy(ctx, info.ID, []string{"example.com:443"})
	assert.ErrorIs(t, err, model.ErrSandboxBooted,
		"a policy staged onto a booting sandbox would never be enforced, so it must be refused, not accepted")

	close(mgr.release)
	<-booting
}

// registerAndBoot registers a task and boots it, for tests that need a
// standing, healthy sandbox to operate on.
func registerAndBoot(ctx context.Context, svc *SandboxService, taskID, worktree string) error {
	if _, err := svc.Register(ctx, regOpts(taskID, worktree)); err != nil {
		return err
	}
	_, err := svc.EnsureRunning(ctx, taskID)
	return err
}

// TestEnsureRunning_JoinerCanceled_ReturnsWithoutAbandoningTheBoot verifies a
// caller that gives up while waiting on someone else's boot is released — and
// that the boot it was waiting on is unaffected, because the boot belongs to
// the registration, not to whichever caller happened to trigger it.
// Refs: MGIT-122
func TestEnsureRunning_JoinerCanceled_ReturnsWithoutAbandoningTheBoot(t *testing.T) {
	mgr := newGatedManager("MGIT-122.B")
	svc := newSvc(t, mgr, &fakeEventAppender{})
	ctx := context.Background()
	_, err := svc.Register(ctx, regOpts("MGIT-122.B", "/wt/b"))
	require.NoError(t, err)

	booting := make(chan struct{})
	go func() {
		defer close(booting)
		_, _ = svc.EnsureRunning(ctx, "MGIT-122.B")
	}()
	mgr.awaitLaunch(t, "MGIT-122.B")

	joinCtx, cancel := context.WithCancel(ctx)
	joined := make(chan error, 1)
	go func() {
		_, jErr := svc.EnsureRunning(joinCtx, "MGIT-122.B")
		joined <- jErr
	}()
	time.Sleep(50 * time.Millisecond)
	cancel()
	select {
	case jErr := <-joined:
		require.ErrorIs(t, jErr, context.Canceled)
	case <-time.After(3 * time.Second):
		t.Fatal("a canceled joiner stayed blocked on another caller's boot")
	}

	close(mgr.release)
	<-booting
	info, err := svc.Status(ctx, "MGIT-122.B")
	require.NoError(t, err)
	assert.Equal(t, model.StateRunning, info.State, "the abandoned joiner took the boot down with it")
	assert.Equal(t, 1, mgr.launchCount("MGIT-122.B"))
}

// TestRemove_CanceledWhileAwaitingBoot_ReturnsAndLeavesTheSandbox verifies the
// teardown wait is not an unbounded one: a caller that cancels while waiting
// for a boot to settle is released, and — because it never acted — the sandbox
// is left registered and usable rather than half-removed. Refs: MGIT-122
func TestRemove_CanceledWhileAwaitingBoot_ReturnsAndLeavesTheSandbox(t *testing.T) {
	mgr := newGatedManager("MGIT-122.B")
	svc := newSvc(t, mgr, &fakeEventAppender{})
	ctx := context.Background()
	_, err := svc.Register(ctx, regOpts("MGIT-122.B", "/wt/b"))
	require.NoError(t, err)

	booting := make(chan struct{})
	go func() {
		defer close(booting)
		_, _ = svc.EnsureRunning(ctx, "MGIT-122.B")
	}()
	mgr.awaitLaunch(t, "MGIT-122.B")

	rmCtx, cancel := context.WithCancel(ctx)
	removed := make(chan error, 1)
	go func() { removed <- svc.Remove(rmCtx, "MGIT-122.B", true) }()
	time.Sleep(50 * time.Millisecond)
	cancel()
	select {
	case rmErr := <-removed:
		require.ErrorIs(t, rmErr, context.Canceled)
	case <-time.After(3 * time.Second):
		t.Fatal("a canceled remove stayed blocked waiting for the boot")
	}

	close(mgr.release)
	<-booting
	stops, removes := mgr.teardowns()
	assert.Zero(t, stops+removes, "a remove that never got past the wait tore something down anyway")
	info, err := svc.Status(ctx, "MGIT-122.B")
	require.NoError(t, err, "the sandbox was dropped by a remove that was canceled before it acted")
	assert.Equal(t, model.StateRunning, info.State)
}
