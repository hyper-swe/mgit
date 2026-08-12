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

// fakeSandboxRegistry is an in-memory stand-in for the durable registry
// (index.Store). It keeps rows in a map so a test can assert what the service
// persisted, and can be made to fail any single operation.
type fakeSandboxRegistry struct {
	rows      map[string]model.SandboxRegistration // by sandbox ID
	upsertErr error
	stateErr  error
	deleteErr error
	listErr   error
	upserts   int
	deletes   int
}

func newFakeRegistry(seed ...model.SandboxRegistration) *fakeSandboxRegistry {
	r := &fakeSandboxRegistry{rows: make(map[string]model.SandboxRegistration)}
	for _, reg := range seed {
		r.rows[reg.Info.ID] = reg
	}
	return r
}

func (r *fakeSandboxRegistry) UpsertSandbox(_ context.Context, reg *model.SandboxRegistration) error {
	r.upserts++
	if r.upsertErr != nil {
		return r.upsertErr
	}
	r.rows[reg.Info.ID] = *reg
	return nil
}

func (r *fakeSandboxRegistry) SetSandboxState(_ context.Context, sandboxID, state string) error {
	if r.stateErr != nil {
		return r.stateErr
	}
	reg, ok := r.rows[sandboxID]
	if !ok {
		return model.ErrSandboxNotFound
	}
	reg.Info.State = state
	r.rows[sandboxID] = reg
	return nil
}

func (r *fakeSandboxRegistry) DeleteSandbox(_ context.Context, sandboxID string) error {
	r.deletes++
	if r.deleteErr != nil {
		return r.deleteErr
	}
	delete(r.rows, sandboxID)
	return nil
}

func (r *fakeSandboxRegistry) ListSandboxes(context.Context) ([]model.SandboxRegistration, error) {
	if r.listErr != nil {
		return nil, r.listErr
	}
	out := make([]model.SandboxRegistration, 0, len(r.rows))
	for _, reg := range r.rows {
		out = append(out, reg)
	}
	return out, nil
}

// persistedRegistration builds a durable row as the registry would hold it.
func persistedRegistration(id, taskID, state string) model.SandboxRegistration {
	return model.SandboxRegistration{
		Info: model.SandboxInfo{
			ID: id, TaskID: taskID, WorktreePath: "/tmp/" + taskID,
			ImageDigest: "sha256:" + strings.Repeat("a", 64), NetworkMode: model.NetworkModeNone,
			State: state, CPUs: 4, MemoryMB: 6144, DiskQuotaMB: 4096,
			CreatedAt: time.Date(2026, 8, 12, 10, 51, 7, 0, time.UTC),
		},
		ImageRef: testImageRef(),
	}
}

// newRehydrateService wires a service with a durable registry, returning all
// three so a test can assert against the audit trail and the durable rows.
func newRehydrateService(t *testing.T, reg *fakeSandboxRegistry) (*SandboxService, *fakeSandboxManager, *fakeEventAppender) {
	t.Helper()
	mgr := &fakeSandboxManager{}
	events := &fakeEventAppender{}
	svc := newSvc(t, mgr, events)
	svc.SetRegistry(reg)
	return svc, mgr, events
}

// TestRegister_WithRegistry_PersistsRegistration is the fix for the reported
// defect at the service boundary: a registration that exists only in daemon
// memory is one daemon exit away from never having happened.
// Refs: FR-17.10, MGIT-102
func TestRegister_WithRegistry_PersistsRegistration(t *testing.T) {
	registry := newFakeRegistry()
	svc, _, _ := newRehydrateService(t, registry)

	info, err := svc.Register(context.Background(), model.SandboxLaunchOptions{
		TaskID: "MGIT-102", WorktreePath: "/tmp/wt", ImageRef: testImageRef(),
		Network: model.NetworkPolicy{Mode: model.NetworkModeNone},
	})
	require.NoError(t, err)

	require.Len(t, registry.rows, 1, "registration must be durable, not daemon-memory only")
	row := registry.rows[info.ID]
	assert.Equal(t, "MGIT-102", row.Info.TaskID)
	assert.Equal(t, "/tmp/wt", row.Info.WorktreePath)
	assert.Equal(t, model.StateCreated, row.Info.State, "a lazily registered sandbox is created, not running")
	assert.Equal(t, testImageRef(), row.ImageRef, "the boot that has not happened yet needs the pinned image")
}

// TestRegister_RegistryWriteFails_AuditDoesNotEndAtCreated closes the audit
// half of the defect at the earliest point it can open: if the registration
// cannot be made durable, the sandbox does not exist, and the trail must say
// so rather than leaving a `created` row asserting a sandbox nothing holds.
// Refs: FR-17.18, MGIT-102
func TestRegister_RegistryWriteFails_AuditDoesNotEndAtCreated(t *testing.T) {
	registry := newFakeRegistry()
	registry.upsertErr = errors.New("disk full")
	svc, _, events := newRehydrateService(t, registry)

	_, err := svc.Register(context.Background(), model.SandboxLaunchOptions{
		TaskID: "MGIT-102", WorktreePath: "/tmp/wt", ImageRef: testImageRef(),
		Network: model.NetworkPolicy{Mode: model.NetworkModeNone},
	})
	require.Error(t, err, "a registration that cannot be made durable must fail closed")

	assert.Equal(t, []string{model.EventCreated, model.EventDestroyed}, events.types(),
		"the trail must not end at created for a sandbox that does not exist")
	_, statusErr := svc.Status(context.Background(), "MGIT-102")
	assert.ErrorIs(t, statusErr, model.ErrSandboxNotFound, "no half-registered sandbox may linger in memory")
}

// TestRehydrate_CreatedNeverBooted_ComesBackAsCreated is the headline
// behavior: the state a `mgit work --sandbox` registration is normally in
// asserts no VM, so it is valid on its face and must return unchanged.
// Refs: FR-17.9, FR-17.10, MGIT-102
func TestRehydrate_CreatedNeverBooted_ComesBackAsCreated(t *testing.T) {
	registry := newFakeRegistry(persistedRegistration("01SB", "MGIT-102", model.StateCreated))
	svc, mgr, events := newRehydrateService(t, registry)

	report, err := svc.Rehydrate(context.Background())
	require.NoError(t, err)
	assert.Equal(t, []string{"MGIT-102"}, report.Recovered)
	assert.Empty(t, report.Discarded)

	info, err := svc.Status(context.Background(), "MGIT-102")
	require.NoError(t, err, "a new daemon must find the sandbox its predecessor registered")
	assert.Equal(t, model.StateCreated, info.State)
	assert.Equal(t, "01SB", info.ID, "the sandbox keeps its identity across the restart")
	assert.Equal(t, 6144, info.MemoryMB, "the resolved caps come back with it")
	assert.Zero(t, mgr.resolves, "a created sandbox asserts no VM, so there is nothing to verify")
	assert.Empty(t, events.types(), "recovering an unchanged registration is not a lifecycle transition")
}

// TestRehydrate_RecordedRunningWithNoVM_DiscardsAndAudits is the honesty
// requirement: reporting `running` for a VM this daemon cannot see would be a
// worse defect than the one being fixed, because it would be confidently
// wrong. Refs: FR-17.18, MGIT-102
func TestRehydrate_RecordedRunningWithNoVM_DiscardsAndAudits(t *testing.T) {
	registry := newFakeRegistry(persistedRegistration("01SB", "MGIT-102", model.StateRunning))
	svc, mgr, events := newRehydrateService(t, registry)
	mgr.resolveErr = errors.New("no such sandbox")

	report, err := svc.Rehydrate(context.Background())
	require.NoError(t, err)
	assert.Empty(t, report.Recovered)
	assert.Equal(t, []string{"MGIT-102"}, report.Discarded)

	assert.Equal(t, 1, mgr.resolves, "a recorded running state must be VERIFIED, never asserted")
	_, statusErr := svc.Status(context.Background(), "MGIT-102")
	assert.ErrorIs(t, statusErr, model.ErrSandboxNotFound)
	assert.Empty(t, registry.rows, "an unverifiable sandbox must not stay in the live registry")

	require.Equal(t, []string{model.EventKilled}, events.types(),
		"a sandbox that ceased to exist must leave a terminal event explaining it")
	assert.Contains(t, events.events[0].Detail, "unsupervised",
		"the record must say WHY it ended, not merely that it did")
}

// TestRehydrate_RecordedRunningVMStillLive_ComesBackRunning covers the other
// side of verification: when the backend can still resolve the VM (a backend
// whose VMs outlive the daemon, or a re-entrant rehydrate), the sandbox comes
// back running and is NOT re-launched. Refs: MGIT-102
func TestRehydrate_RecordedRunningVMStillLive_ComesBackRunning(t *testing.T) {
	registry := newFakeRegistry(persistedRegistration("01SB", "MGIT-102", model.StateRunning))
	svc, mgr, _ := newRehydrateService(t, registry)
	mgr.resolveInfo = &model.SandboxInfo{
		ID: "01SB", TaskID: "MGIT-102", WorktreePath: "/tmp/MGIT-102",
		Backend: model.BackendKVM, State: model.StateRunning,
	}

	report, err := svc.Rehydrate(context.Background())
	require.NoError(t, err)
	assert.Equal(t, []string{"MGIT-102"}, report.Recovered)

	info, err := svc.EnsureRunning(context.Background(), "MGIT-102")
	require.NoError(t, err)
	assert.Equal(t, model.StateRunning, info.State)
	assert.Zero(t, mgr.launches, "a verified-running sandbox must not be booted a second time")
}

// TestRehydrate_RecordedSuspendedWithNoVM_DiscardsAndAudits — a suspended
// sandbox also asserts a VM (paused, disk intact). If it cannot be verified it
// is gone, and saying "suspended" would promise a resume that cannot happen.
func TestRehydrate_RecordedSuspendedWithNoVM_DiscardsAndAudits(t *testing.T) {
	registry := newFakeRegistry(persistedRegistration("01SB", "MGIT-102", model.StateSuspended))
	svc, mgr, events := newRehydrateService(t, registry)
	mgr.resolveErr = errors.New("no such sandbox")

	report, err := svc.Rehydrate(context.Background())
	require.NoError(t, err)
	assert.Equal(t, []string{"MGIT-102"}, report.Discarded)
	assert.Equal(t, []string{model.EventKilled}, events.types())
}

// TestRehydrate_TwiceOverSameRegistry_IsIdempotent — the daemon may rehydrate
// more than once (re-entrancy, a future reload); doing so must not duplicate
// registrations or re-audit anything.
func TestRehydrate_TwiceOverSameRegistry_IsIdempotent(t *testing.T) {
	registry := newFakeRegistry(persistedRegistration("01SB", "MGIT-102", model.StateCreated))
	svc, _, events := newRehydrateService(t, registry)

	first, err := svc.Rehydrate(context.Background())
	require.NoError(t, err)
	second, err := svc.Rehydrate(context.Background())
	require.NoError(t, err)

	assert.Equal(t, []string{"MGIT-102"}, first.Recovered)
	assert.Empty(t, second.Recovered, "an already-live registration is not recovered twice")
	assert.Empty(t, events.types())
	list, err := svc.List(context.Background())
	require.NoError(t, err)
	assert.Len(t, list, 1)
}

// TestRehydrate_PreservesTTLDeadline — the TTL deadline is host-clock absolute,
// so a restart must not silently extend a sandbox's life by restarting its
// clock. Refs: FR-17.9
func TestRehydrate_PreservesTTLDeadline(t *testing.T) {
	row := persistedRegistration("01SB", "MGIT-102", model.StateCreated)
	// The service clock is fixed at the epoch in these tests, so any deadline
	// at or before it is already past.
	row.Info.ExpiresAt = time.Unix(0, 0).UTC().Add(-time.Minute)
	row.TTL = time.Hour
	registry := newFakeRegistry(row)
	svc, _, events := newRehydrateService(t, registry)

	_, err := svc.Rehydrate(context.Background())
	require.NoError(t, err)

	reaped, err := svc.ReapExpired(context.Background())
	require.NoError(t, err)
	assert.Equal(t, []string{"MGIT-102"}, reaped, "a rehydrated sandbox past its TTL is reaped, not renewed")
	assert.Equal(t, []string{model.EventTTLExpired}, events.types())
	assert.Empty(t, registry.rows, "reaping clears the durable row too")
}

// TestRehydrate_NoRegistryWired_IsANoOp keeps the service usable in wirings
// that have no durable registry (unit tests, greet-only daemons) without
// pretending anything was recovered.
func TestRehydrate_NoRegistryWired_IsANoOp(t *testing.T) {
	mgr := &fakeSandboxManager{}
	svc := newSvc(t, mgr, &fakeEventAppender{})

	report, err := svc.Rehydrate(context.Background())
	require.NoError(t, err)
	assert.Empty(t, report.Recovered)
	assert.Empty(t, report.Discarded)
}

// TestRehydrate_RegistryUnreadable_ReturnsError — a daemon that cannot read
// the registry must say so, not start serving as if the roster were empty.
// Starting empty is exactly how the reported defect presented.
func TestRehydrate_RegistryUnreadable_ReturnsError(t *testing.T) {
	registry := newFakeRegistry()
	registry.listErr = errors.New("database is locked")
	svc, _, _ := newRehydrateService(t, registry)

	_, err := svc.Rehydrate(context.Background())
	require.Error(t, err)
	assert.Contains(t, err.Error(), "database is locked")
}

// TestEnsureRunning_AfterBoot_PersistsRunningState — the next daemon must
// reconcile against the last state actually observed, so a boot has to be
// written through to the registry.
func TestEnsureRunning_AfterBoot_PersistsRunningState(t *testing.T) {
	registry := newFakeRegistry()
	svc, _, _ := newRehydrateService(t, registry)
	info, err := svc.Register(context.Background(), model.SandboxLaunchOptions{
		TaskID: "MGIT-102", WorktreePath: "/tmp/wt", ImageRef: testImageRef(),
		Network: model.NetworkPolicy{Mode: model.NetworkModeNone},
	})
	require.NoError(t, err)

	_, err = svc.EnsureRunning(context.Background(), "MGIT-102")
	require.NoError(t, err)

	assert.Equal(t, model.StateRunning, registry.rows[info.ID].Info.State)
}

// TestSuspendIdle_PersistsSuspendedState — same reasoning as the boot: an
// idle-suspended sandbox that the registry still calls running would be
// reconciled against the wrong claim.
func TestSuspendIdle_PersistsSuspendedState(t *testing.T) {
	registry := newFakeRegistry()
	svc, _, _ := newRehydrateService(t, registry)
	info, err := svc.Register(context.Background(), model.SandboxLaunchOptions{
		TaskID: "MGIT-102", WorktreePath: "/tmp/wt", ImageRef: testImageRef(),
		Network: model.NetworkPolicy{Mode: model.NetworkModeNone},
	})
	require.NoError(t, err)
	_, err = svc.EnsureRunning(context.Background(), "MGIT-102")
	require.NoError(t, err)

	suspended, err := svc.SuspendIdle(context.Background(), 0)
	require.NoError(t, err)
	require.Equal(t, []string{"MGIT-102"}, suspended)
	assert.Equal(t, model.StateSuspended, registry.rows[info.ID].Info.State)
}

// TestRemove_PersistedSandbox_DeletesRegistryRowAndAudits — teardown is the
// GENUINELY-destroyed case: the row goes, and the trail ends in a terminal
// event, so no record asserts a sandbox that no longer exists.
func TestRemove_PersistedSandbox_DeletesRegistryRowAndAudits(t *testing.T) {
	registry := newFakeRegistry()
	svc, _, events := newRehydrateService(t, registry)
	_, err := svc.Register(context.Background(), model.SandboxLaunchOptions{
		TaskID: "MGIT-102", WorktreePath: "/tmp/wt", ImageRef: testImageRef(),
		Network: model.NetworkPolicy{Mode: model.NetworkModeNone},
	})
	require.NoError(t, err)

	require.NoError(t, svc.Remove(context.Background(), "MGIT-102", true))

	assert.Empty(t, registry.rows, "a torn-down sandbox must leave no live row to rehydrate")
	assert.Equal(t, []string{model.EventCreated, model.EventDestroyed}, events.types())
}

// TestRemove_AfterRehydrate_TearsDownTheRecoveredSandbox proves the recovered
// registration is a real one, not a read-only shadow: it can be torn down by
// the daemon that adopted it.
func TestRemove_AfterRehydrate_TearsDownTheRecoveredSandbox(t *testing.T) {
	registry := newFakeRegistry(persistedRegistration("01SB", "MGIT-102", model.StateCreated))
	svc, _, events := newRehydrateService(t, registry)
	_, err := svc.Rehydrate(context.Background())
	require.NoError(t, err)

	require.NoError(t, svc.Remove(context.Background(), "MGIT-102", true))
	assert.Empty(t, registry.rows)
	assert.Equal(t, []string{model.EventDestroyed}, events.types())
}

// TestRehydrate_BackendReportedState_IsWhatComesBack is the verification rule
// stated as a table: for a row claiming a VM, the state that comes back is the
// one the BACKEND reports — never the one the row claimed. A backend that
// knows the sandbox as over (landed/destroyed) ends it, whatever the row said.
// Refs: MGIT-102
func TestRehydrate_BackendReportedState_IsWhatComesBack(t *testing.T) {
	tests := []struct {
		name        string
		recorded    string
		backend     *model.SandboxInfo
		wantState   string
		wantDropped bool
	}{
		{
			name: "backend_says_running", recorded: model.StateRunning,
			backend:   &model.SandboxInfo{ID: "01SB", State: model.StateRunning},
			wantState: model.StateRunning,
		},
		{
			name: "backend_says_suspended", recorded: model.StateRunning,
			backend:   &model.SandboxInfo{ID: "01SB", State: model.StateSuspended},
			wantState: model.StateSuspended,
		},
		{
			name: "backend_says_created", recorded: model.StateSuspended,
			backend:   &model.SandboxInfo{ID: "01SB", State: model.StateCreated},
			wantState: model.StateCreated,
		},
		{
			name: "backend_says_destroyed", recorded: model.StateRunning,
			backend: &model.SandboxInfo{ID: "01SB", State: model.StateDestroyed}, wantDropped: true,
		},
		{
			name: "backend_says_landed", recorded: model.StateRunning,
			backend: &model.SandboxInfo{ID: "01SB", State: model.StateLanded}, wantDropped: true,
		},
		{
			name: "backend_knows_nothing", recorded: model.StateRunning,
			backend: nil, wantDropped: true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			registry := newFakeRegistry(persistedRegistration("01SB", "MGIT-102", tt.recorded))
			svc, mgr, events := newRehydrateService(t, registry)
			mgr.resolveInfo = tt.backend

			_, err := svc.Rehydrate(context.Background())
			require.NoError(t, err)

			info, statusErr := svc.Status(context.Background(), "MGIT-102")
			if tt.wantDropped {
				assert.ErrorIs(t, statusErr, model.ErrSandboxNotFound)
				assert.Equal(t, []string{model.EventKilled}, events.types(),
					"a discarded sandbox always leaves a terminal event")
				return
			}
			require.NoError(t, statusErr)
			assert.Equal(t, tt.wantState, info.State)
			assert.Empty(t, events.types(), "adopting a verified sandbox is not a transition")
		})
	}
}

// TestRehydrate_AuditFailureOnLostSandbox_StopsAndSurfaces — the audit is
// written BEFORE the durable row is dropped, so an audit failure must abort the
// discard rather than delete the row silently. Dropping first could leave a
// sandbox with no terminal event at all, which is the misleading trail this
// ticket exists to close. Refs: FR-17.18, MGIT-102
func TestRehydrate_AuditFailureOnLostSandbox_StopsAndSurfaces(t *testing.T) {
	registry := newFakeRegistry(persistedRegistration("01SB", "MGIT-102", model.StateRunning))
	svc, mgr, events := newRehydrateService(t, registry)
	mgr.resolveErr = errors.New("no such sandbox")
	events.failNth = 1

	_, err := svc.Rehydrate(context.Background())
	require.Error(t, err)
	assert.Contains(t, err.Error(), "audit lost sandbox")
	assert.Len(t, registry.rows, 1, "the row must survive an audit failure, so the discard is retried")
	assert.Zero(t, registry.deletes)
}

// TestRehydrate_ThenEnsureRunning_BootsWithTheOriginalOptions closes the loop
// the ticket actually cares about: a recovered registration must be USABLE, not
// merely visible. The boot that finally happens must use the options the
// sandbox was registered with — same host-assigned ID, same pinned image, same
// egress posture, same caps — or a sandbox would come back as something other
// than what the user asked for. Refs: FR-17.10, MGIT-102
func TestRehydrate_ThenEnsureRunning_BootsWithTheOriginalOptions(t *testing.T) {
	row := persistedRegistration("01SB", "MGIT-102", model.StateCreated)
	row.Info.NetworkMode = model.NetworkModeAllowlist
	row.Info.NetworkAllowlist = []string{"proxy.golang.org"}
	registry := newFakeRegistry(row)
	svc, mgr, events := newRehydrateService(t, registry)

	_, err := svc.Rehydrate(context.Background())
	require.NoError(t, err)

	info, err := svc.EnsureRunning(context.Background(), "MGIT-102")
	require.NoError(t, err)
	assert.Equal(t, 1, mgr.launches, "a recovered created sandbox boots on first use, as it always would have")
	assert.Equal(t, "01SB", mgr.lastOpts.SandboxID, "the sandbox keeps the ID it was registered with")
	assert.Equal(t, testImageRef(), mgr.lastOpts.ImageRef, "and the image it was pinned to")
	assert.Equal(t, model.NetworkModeAllowlist, mgr.lastOpts.Network.Mode, "and its egress posture")
	assert.Equal(t, []string{"proxy.golang.org"}, mgr.lastOpts.Network.Allowlist)
	assert.Equal(t, 6144, mgr.lastOpts.MemoryMB, "and its resolved ceiling")
	assert.Equal(t, model.StateRunning, info.State)
	assert.Equal(t, []string{model.EventResumed}, events.types())
	assert.Equal(t, model.StateRunning, registry.rows["01SB"].Info.State,
		"the boot is written back through to the durable registry")
}
