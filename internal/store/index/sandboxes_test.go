package index

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/hyper-swe/mgit/internal/model"
)

// testRegistration builds a fully-populated registration so round-trip tests
// assert every persisted column, not just the identity ones.
func testRegistration(sandboxID, taskID, path string) *model.SandboxRegistration {
	created := time.Date(2026, 8, 12, 10, 51, 7, 0, time.UTC)
	return &model.SandboxRegistration{
		Info: model.SandboxInfo{
			ID: sandboxID, TaskID: taskID, WorktreePath: path,
			Backend:          model.BackendKVM,
			ImageDigest:      "sha256:" + testHex64('a'),
			NetworkMode:      model.NetworkModeAllowlist,
			NetworkAllowlist: []string{"registry.npmjs.org", "proxy.golang.org"},
			State:            model.StateCreated,
			CPUs:             4, MemoryMB: 6144, DiskQuotaMB: 4096,
			CreatedAt:    created,
			ExpiresAt:    created.Add(2 * time.Hour),
			PublishPorts: []model.PortPublish{{HostPort: 8080, GuestPort: 80}},
		},
		ImageRef: "base@sha256:" + testHex64('a'),
		TTL:      2 * time.Hour,
	}
}

func testHex64(c byte) string {
	b := make([]byte, 64)
	for i := range b {
		b[i] = c
	}
	return string(b)
}

func newSandboxRegistryStore(t *testing.T) (*Store, context.Context) {
	t.Helper()
	clock := func() time.Time { return time.Date(2026, 8, 12, 12, 0, 0, 0, time.UTC) }
	store, err := New(t.TempDir()+"/sandbox-index.db", clock)
	require.NoError(t, err)
	t.Cleanup(func() { _ = store.Close() })
	return store, context.Background()
}

// TestUpsertSandbox_ValidRegistration_RoundTripsEveryField is the core
// durability contract: everything the daemon needs to bring a sandbox back
// must survive the write, including the launch inputs SandboxInfo does not
// carry (image ref, TTL). Refs: FR-17.10, MGIT-102
func TestUpsertSandbox_ValidRegistration_RoundTripsEveryField(t *testing.T) {
	store, ctx := newSandboxRegistryStore(t)
	want := testRegistration("01SANDBOX", "MGIT-102", "/tmp/wt")

	require.NoError(t, store.UpsertSandbox(ctx, want))

	got, err := store.ListSandboxes(ctx)
	require.NoError(t, err)
	require.Len(t, got, 1)
	assert.Equal(t, want.Info.ID, got[0].Info.ID)
	assert.Equal(t, want.Info.TaskID, got[0].Info.TaskID)
	assert.Equal(t, want.Info.WorktreePath, got[0].Info.WorktreePath)
	assert.Equal(t, want.Info.Backend, got[0].Info.Backend)
	assert.Equal(t, want.Info.ImageDigest, got[0].Info.ImageDigest)
	assert.Equal(t, want.Info.NetworkMode, got[0].Info.NetworkMode)
	assert.Equal(t, want.Info.NetworkAllowlist, got[0].Info.NetworkAllowlist)
	assert.Equal(t, want.Info.PublishPorts, got[0].Info.PublishPorts)
	assert.Equal(t, want.Info.State, got[0].Info.State)
	assert.Equal(t, want.Info.CPUs, got[0].Info.CPUs)
	assert.Equal(t, want.Info.MemoryMB, got[0].Info.MemoryMB)
	assert.Equal(t, want.Info.DiskQuotaMB, got[0].Info.DiskQuotaMB)
	assert.True(t, want.Info.CreatedAt.Equal(got[0].Info.CreatedAt))
	assert.True(t, want.Info.ExpiresAt.Equal(got[0].Info.ExpiresAt))
	assert.Equal(t, want.ImageRef, got[0].ImageRef)
	assert.Equal(t, want.TTL, got[0].TTL)
}

// TestUpsertSandbox_AfterStoreReopen_RegistrationSurvives is the property the
// whole ticket turns on: the registration outlives the process that wrote it.
// Refs: MGIT-102
func TestUpsertSandbox_AfterStoreReopen_RegistrationSurvives(t *testing.T) {
	dbPath := t.TempDir() + "/sandbox-index.db"
	clock := func() time.Time { return time.Date(2026, 8, 12, 12, 0, 0, 0, time.UTC) }
	ctx := context.Background()

	first, err := New(dbPath, clock)
	require.NoError(t, err)
	require.NoError(t, first.UpsertSandbox(ctx, testRegistration("01SANDBOX", "MGIT-102", "/tmp/wt")))
	require.NoError(t, first.Close())

	second, err := New(dbPath, clock)
	require.NoError(t, err)
	defer func() { _ = second.Close() }()

	got, err := second.ListSandboxes(ctx)
	require.NoError(t, err)
	require.Len(t, got, 1, "a registration written by one process must be readable by the next")
	assert.Equal(t, "MGIT-102", got[0].Info.TaskID)
	assert.Equal(t, model.StateCreated, got[0].Info.State)
}

// TestUpsertSandbox_ExclusivityViolations_Rejected pins FR-17.1 at the durable
// layer: one task and one worktree may hold at most one sandbox, so a second
// row cannot claim either even if the in-memory check were bypassed.
func TestUpsertSandbox_ExclusivityViolations_Rejected(t *testing.T) {
	tests := []struct {
		name              string
		sandboxID, taskID string
		path              string
	}{
		{name: "duplicate_task", sandboxID: "01OTHER", taskID: "MGIT-102", path: "/tmp/other"},
		{name: "duplicate_worktree", sandboxID: "01OTHER", taskID: "MGIT-999", path: "/tmp/wt"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			store, ctx := newSandboxRegistryStore(t)
			require.NoError(t, store.UpsertSandbox(ctx, testRegistration("01SANDBOX", "MGIT-102", "/tmp/wt")))

			err := store.UpsertSandbox(ctx, testRegistration(tt.sandboxID, tt.taskID, tt.path))
			require.Error(t, err)

			got, listErr := store.ListSandboxes(ctx)
			require.NoError(t, listErr)
			assert.Len(t, got, 1, "the rejected registration must not have landed")
		})
	}
}

// TestUpsertSandbox_SameSandboxTwice_UpdatesInPlace keeps the registry a LIVE
// view (one row per sandbox), never an accreting log — the append-only law
// belongs to sandbox_events, which records the transitions.
func TestUpsertSandbox_SameSandboxTwice_UpdatesInPlace(t *testing.T) {
	store, ctx := newSandboxRegistryStore(t)
	reg := testRegistration("01SANDBOX", "MGIT-102", "/tmp/wt")
	require.NoError(t, store.UpsertSandbox(ctx, reg))

	reg.Info.State = model.StateRunning
	require.NoError(t, store.UpsertSandbox(ctx, reg))

	got, err := store.ListSandboxes(ctx)
	require.NoError(t, err)
	require.Len(t, got, 1)
	assert.Equal(t, model.StateRunning, got[0].Info.State)
}

// TestUpsertSandbox_InvalidRegistration_Rejected keeps hollow rows out: a row
// in this table asserts a sandbox exists, and one that cannot be launched
// would resurrect a sandbox nothing can serve.
func TestUpsertSandbox_InvalidRegistration_Rejected(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(*model.SandboxRegistration)
	}{
		{name: "no_id", mutate: func(r *model.SandboxRegistration) { r.Info.ID = "" }},
		{name: "no_task", mutate: func(r *model.SandboxRegistration) { r.Info.TaskID = "" }},
		{name: "unpinned_image", mutate: func(r *model.SandboxRegistration) { r.ImageRef = "base:latest" }},
		{name: "no_state", mutate: func(r *model.SandboxRegistration) { r.Info.State = "" }},
		{name: "unknown_state", mutate: func(r *model.SandboxRegistration) { r.Info.State = "zombie" }},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			store, ctx := newSandboxRegistryStore(t)
			reg := testRegistration("01SANDBOX", "MGIT-102", "/tmp/wt")
			tt.mutate(reg)

			require.Error(t, store.UpsertSandbox(ctx, reg))
			got, err := store.ListSandboxes(ctx)
			require.NoError(t, err)
			assert.Empty(t, got)
		})
	}
}

// TestSetSandboxState_KnownSandbox_PersistsState covers the state transitions
// a running daemon records so the next daemon reconciles against what was
// actually last observed, not against the registration-time state.
func TestSetSandboxState_KnownSandbox_PersistsState(t *testing.T) {
	store, ctx := newSandboxRegistryStore(t)
	require.NoError(t, store.UpsertSandbox(ctx, testRegistration("01SANDBOX", "MGIT-102", "/tmp/wt")))

	require.NoError(t, store.SetSandboxState(ctx, "01SANDBOX", model.StateRunning))

	got, err := store.ListSandboxes(ctx)
	require.NoError(t, err)
	require.Len(t, got, 1)
	assert.Equal(t, model.StateRunning, got[0].Info.State)
}

// TestSetSandboxState_UnknownSandbox_ReturnsNotFound refuses to silently
// no-op: a state write for a sandbox with no row means the caller and the
// registry disagree about what exists.
func TestSetSandboxState_UnknownSandbox_ReturnsNotFound(t *testing.T) {
	store, ctx := newSandboxRegistryStore(t)
	err := store.SetSandboxState(ctx, "01MISSING", model.StateRunning)
	assert.ErrorIs(t, err, model.ErrSandboxNotFound)
}

// TestSetSandboxState_UnknownState_Rejected closes the vocabulary at the
// durable boundary, as sandbox_events does for event types.
func TestSetSandboxState_UnknownState_Rejected(t *testing.T) {
	store, ctx := newSandboxRegistryStore(t)
	require.NoError(t, store.UpsertSandbox(ctx, testRegistration("01SANDBOX", "MGIT-102", "/tmp/wt")))
	assert.Error(t, store.SetSandboxState(ctx, "01SANDBOX", "zombie"))
}

// TestDeleteSandbox_KnownSandbox_RemovesRow — teardown frees the task and
// worktree bindings, so the row must go with the sandbox.
func TestDeleteSandbox_KnownSandbox_RemovesRow(t *testing.T) {
	store, ctx := newSandboxRegistryStore(t)
	require.NoError(t, store.UpsertSandbox(ctx, testRegistration("01SANDBOX", "MGIT-102", "/tmp/wt")))

	require.NoError(t, store.DeleteSandbox(ctx, "01SANDBOX"))

	got, err := store.ListSandboxes(ctx)
	require.NoError(t, err)
	assert.Empty(t, got)
}

// TestDeleteSandbox_UnknownSandbox_ReturnsNotFound — same reasoning as
// SetSandboxState: a silent no-op hides a disagreement.
func TestDeleteSandbox_UnknownSandbox_ReturnsNotFound(t *testing.T) {
	store, ctx := newSandboxRegistryStore(t)
	assert.ErrorIs(t, store.DeleteSandbox(ctx, "01MISSING"), model.ErrSandboxNotFound)
}

// TestListSandboxes_Empty_ReturnsNoRegistrations — a fresh daemon on a repo
// that never registered anything rehydrates nothing, without erroring.
func TestListSandboxes_Empty_ReturnsNoRegistrations(t *testing.T) {
	store, ctx := newSandboxRegistryStore(t)
	got, err := store.ListSandboxes(ctx)
	require.NoError(t, err)
	assert.Empty(t, got)
}

// TestListSandboxes_MultipleSandboxes_SortedByTask keeps daemon-start
// rehydration deterministic (and the operator's list stable).
func TestListSandboxes_MultipleSandboxes_SortedByTask(t *testing.T) {
	store, ctx := newSandboxRegistryStore(t)
	require.NoError(t, store.UpsertSandbox(ctx, testRegistration("01B", "MGIT-200", "/tmp/b")))
	require.NoError(t, store.UpsertSandbox(ctx, testRegistration("01A", "MGIT-100", "/tmp/a")))

	got, err := store.ListSandboxes(ctx)
	require.NoError(t, err)
	require.Len(t, got, 2)
	assert.Equal(t, "MGIT-100", got[0].Info.TaskID)
	assert.Equal(t, "MGIT-200", got[1].Info.TaskID)
}

// TestSandboxRegistry_Lifecycle_NeverTouchesTaskCommits guards the law that
// motivated the separation: the new registry is mutable live state, and
// task_commits is append-only audit. A registry write that reached it would
// be an append-only violation. Refs: FR-12, MGIT-102
func TestSandboxRegistry_Lifecycle_NeverTouchesTaskCommits(t *testing.T) {
	store, ctx := newSandboxRegistryStore(t)
	require.NoError(t, store.AppendTaskCommit(ctx, TaskCommitInsert{
		TaskID: "MGIT-102", CommitHash: testHex40('a'), ContentHash: testHex64('b'),
	}))
	before, err := store.GetTaskCommits(ctx, "MGIT-102")
	require.NoError(t, err)

	reg := testRegistration("01SANDBOX", "MGIT-102", "/tmp/wt")
	require.NoError(t, store.UpsertSandbox(ctx, reg))
	require.NoError(t, store.SetSandboxState(ctx, "01SANDBOX", model.StateRunning))
	require.NoError(t, store.DeleteSandbox(ctx, "01SANDBOX"))

	after, err := store.GetTaskCommits(ctx, "MGIT-102")
	require.NoError(t, err)
	assert.Equal(t, before, after, "the sandbox registry must never write task_commits")
}

func testHex40(c byte) string {
	b := make([]byte, 40)
	for i := range b {
		b[i] = c
	}
	return string(b)
}

// TestUpsertSandbox_ConfinedAgentNoTTL_RoundTrips covers the other end of the
// value ranges: the T2 confined-agent flag set, and a sandbox with no TTL at
// all (a zero deadline must come back as "no deadline", not as year 1).
func TestUpsertSandbox_ConfinedAgentNoTTL_RoundTrips(t *testing.T) {
	store, ctx := newSandboxRegistryStore(t)
	reg := testRegistration("01SANDBOX", "MGIT-102", "/tmp/wt")
	reg.ConfineAgent = true
	reg.TTL = 0
	reg.Info.ExpiresAt = time.Time{}
	reg.Info.NetworkAllowlist = nil
	reg.Info.PublishPorts = nil

	require.NoError(t, store.UpsertSandbox(ctx, reg))

	got, err := store.ListSandboxes(ctx)
	require.NoError(t, err)
	require.Len(t, got, 1)
	assert.True(t, got[0].ConfineAgent, "the confined-agent topology must survive the restart")
	assert.Zero(t, got[0].TTL)
	assert.True(t, got[0].Info.ExpiresAt.IsZero(), "no TTL must come back as no deadline, not year 1")
	assert.Empty(t, got[0].Info.NetworkAllowlist)
	assert.Empty(t, got[0].Info.PublishPorts)
}

// TestListSandboxes_CorruptedRow_ReportsCorruption — a registry row that cannot
// be decoded must NOT be rehydrated as a half-configured sandbox. A sandbox
// brought back with a lost allowlist would run with the wrong containment,
// which is worse than one not brought back at all. Refs: SEC-04, MGIT-102
func TestListSandboxes_CorruptedRow_ReportsCorruption(t *testing.T) {
	// Each case carries its whole statement as a literal (rather than a column
	// name spliced into one) so the corruption fixture itself obeys the
	// no-concatenated-SQL law.
	tests := []struct {
		name    string
		corrupt string
		value   string
	}{
		{name: "bad_allowlist_json", value: "{not json",
			corrupt: `UPDATE sandboxes SET network_allowlist = ? WHERE sandbox_id = ?`},
		{name: "bad_publish_ports_json", value: "[[[",
			corrupt: `UPDATE sandboxes SET publish_ports = ? WHERE sandbox_id = ?`},
		{name: "bad_created_at", value: "yesterday",
			corrupt: `UPDATE sandboxes SET created_at = ? WHERE sandbox_id = ?`},
		{name: "bad_expires_at", value: "soon",
			corrupt: `UPDATE sandboxes SET expires_at = ? WHERE sandbox_id = ?`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			store, ctx := newSandboxRegistryStore(t)
			require.NoError(t, store.UpsertSandbox(ctx, testRegistration("01SANDBOX", "MGIT-102", "/tmp/wt")))
			// Corrupt one column directly: no service path can write this, which
			// is exactly why the read must defend against it.
			_, err := store.writeDB.ExecContext(ctx, tt.corrupt, tt.value, "01SANDBOX")
			require.NoError(t, err)

			_, err = store.ListSandboxes(ctx)
			require.Error(t, err)
			assert.ErrorIs(t, err, model.ErrIndexCorrupted)
		})
	}
}
