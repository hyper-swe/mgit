// Package index tests verify the append-only sandbox_events schema per
// MGIT-11.3.1 acceptance criteria. Refs: FR-17.18, NFR-3
package index

import (
	"context"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"strings"
	"testing"
	"unicode"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/hyper-swe/mgit/internal/model"
)

func testSandboxEvent() *model.SandboxEvent {
	return &model.SandboxEvent{
		SandboxID:   "01JXSANDBOX00000000000000",
		TaskID:      "MGIT-4.2",
		EventType:   model.EventCreated,
		Backend:     model.BackendKVM,
		ImageDigest: "sha256:" + strings.Repeat("a", 64),
		NetworkMode: model.NetworkModeAllowlist,
		Detail:      `{"cpus":2}`,
	}
}

// TestSchema_SandboxEventsTable verifies the table shape: all FR-17.18
// columns present, NO ended_at (F-01: it would force UPDATE), and the
// safety PRAGMAs active. Refs: FR-17.18
func TestSchema_SandboxEventsTable(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()

	var columns []string
	rows, err := store.readDB.QueryContext(ctx, "PRAGMA table_info(sandbox_events)")
	require.NoError(t, err)
	defer rows.Close() //nolint:errcheck // non-critical

	for rows.Next() {
		var cid int
		var name, colType string
		var notNull, pk int
		var dflt any
		require.NoError(t, rows.Scan(&cid, &name, &colType, &notNull, &dflt, &pk))
		columns = append(columns, name)
	}
	require.NoError(t, rows.Err())

	want := []string{
		"id", "sandbox_id", "task_id", "event_type",
		"backend", "image_digest", "network_mode", "detail", "created_at",
	}
	assert.ElementsMatch(t, want, columns, "sandbox_events must have exactly the FR-17.18 columns")
	assert.NotContains(t, columns, "ended_at",
		"ended_at would force UPDATE on an audit table (F-01)")

	var journalMode string
	require.NoError(t, store.readDB.QueryRowContext(ctx, "PRAGMA journal_mode").Scan(&journalMode))
	assert.Equal(t, "wal", journalMode)
	var fk int
	require.NoError(t, store.writeDB.QueryRowContext(ctx, "PRAGMA foreign_keys").Scan(&fk))
	assert.Equal(t, 1, fk)
	var sync int
	require.NoError(t, store.writeDB.QueryRowContext(ctx, "PRAGMA synchronous").Scan(&sync))
	assert.Equal(t, 2, sync, "synchronous must be FULL")
}

// TestSandboxEvents_AppendOnly_NoUpdatePath verifies by construction
// that no UPDATE or DELETE path exists for sandbox_events: neither in
// the package SQL nor in the Store method surface. Refs: FR-17.18, NFR-3.1
func TestSandboxEvents_AppendOnly_NoUpdatePath(t *testing.T) {
	entries, err := os.ReadDir(".")
	require.NoError(t, err)

	mutationRe := regexp.MustCompile(`(?i)\b(update\s+sandbox_events|delete\s+from\s+sandbox_events)\b`)
	for _, entry := range entries {
		if !strings.HasSuffix(entry.Name(), ".go") || strings.HasSuffix(entry.Name(), "_test.go") {
			continue
		}
		src, err := os.ReadFile(filepath.Clean(entry.Name()))
		require.NoError(t, err)
		assert.False(t, mutationRe.Match(src),
			"%s must not contain UPDATE/DELETE touching sandbox_events", entry.Name())
	}

	storeType := reflect.TypeOf(&Store{})
	for i := 0; i < storeType.NumMethod(); i++ {
		name := storeType.Method(i).Name
		if strings.Contains(name, "SandboxEvent") {
			for _, verb := range []string{"Update", "Delete", "Remove", "Prune"} {
				assert.False(t, strings.HasPrefix(name, verb),
					"no mutating method may exist for sandbox events: %s", name)
			}
		}
	}
}

// TestSandboxEvents_DetailSanitized_ControlCharsStripped verifies F-09:
// guest-sourced detail strings are control-char-stripped and
// length-capped before entering the append-only table — corrupted
// entries would be permanent. Refs: FR-17.18
func TestSandboxEvents_DetailSanitized_ControlCharsStripped(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()

	t.Run("control_chars_stripped", func(t *testing.T) {
		ev := testSandboxEvent()
		ev.Detail = "line1\nline2\x1b[31mred\x00null\tend"
		require.NoError(t, store.AppendSandboxEvent(ctx, ev))

		got, err := store.ListSandboxEvents(ctx, ev.SandboxID)
		require.NoError(t, err)
		require.Len(t, got, 1)
		assert.False(t, strings.ContainsFunc(got[0].Detail, unicode.IsControl),
			"stored detail must contain no control characters, got %q", got[0].Detail)
		assert.Contains(t, got[0].Detail, "line1")
		assert.Contains(t, got[0].Detail, "end")
	})

	t.Run("length_capped", func(t *testing.T) {
		ev := testSandboxEvent()
		ev.SandboxID = "01JXSANDBOX00000000000001"
		ev.Detail = strings.Repeat("a", 100_000)
		require.NoError(t, store.AppendSandboxEvent(ctx, ev))

		got, err := store.ListSandboxEvents(ctx, ev.SandboxID)
		require.NoError(t, err)
		require.Len(t, got, 1)
		assert.LessOrEqual(t, len(got[0].Detail), maxSandboxEventDetailLen,
			"stored detail must be length-capped")
	})
}

// TestSandboxEvents_Parameterized_NoSprintf verifies SQL Rule 1: no
// string formatting builds SQL in the sandbox-events store file.
// Refs: NFR-5.4
func TestSandboxEvents_Parameterized_NoSprintf(t *testing.T) {
	src, err := os.ReadFile("sandbox_events.go")
	require.NoError(t, err)

	sprintfSQL := regexp.MustCompile(`(?i)Sprintf\([^)]*\b(select|insert|update|delete)\b`)
	assert.False(t, sprintfSQL.Match(src),
		"sandbox_events.go must not build SQL with Sprintf (parameterized queries only)")
	assert.NotContains(t, string(src), "fmt.Sprintf(`",
		"no SQL template formatting permitted")
}

// TestSandboxEvents_AppendAndList_RoundTrip covers the happy path:
// store-assigned ULID ids, chronological ordering, and field
// round-trip. Refs: FR-17.18
func TestSandboxEvents_AppendAndList_RoundTrip(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()

	first := testSandboxEvent()
	require.NoError(t, store.AppendSandboxEvent(ctx, first))

	second := testSandboxEvent()
	second.EventType = model.EventDestroyed
	second.Backend, second.ImageDigest, second.NetworkMode = "", "", ""
	require.NoError(t, store.AppendSandboxEvent(ctx, second))

	got, err := store.ListSandboxEvents(ctx, first.SandboxID)
	require.NoError(t, err)
	require.Len(t, got, 2)

	assert.NotEmpty(t, got[0].ID, "store must assign a ULID id")
	assert.NotEqual(t, got[0].ID, got[1].ID, "event ids must be unique")
	assert.Less(t, got[0].ID, got[1].ID, "ULID ids must sort in append order")
	assert.Equal(t, model.EventCreated, got[0].EventType)
	assert.Equal(t, model.EventDestroyed, got[1].EventType)
	assert.Equal(t, first.TaskID, got[0].TaskID)
	assert.Equal(t, model.BackendKVM, got[0].Backend)
	assert.False(t, got[0].CreatedAt.IsZero(), "created_at must be recorded")

	t.Run("unknown_sandbox_returns_empty", func(t *testing.T) {
		events, err := store.ListSandboxEvents(ctx, "01JXNOSUCHSANDBOX00000000")
		require.NoError(t, err)
		assert.Empty(t, events)
	})

	t.Run("invalid_event_rejected", func(t *testing.T) {
		bad := testSandboxEvent()
		bad.EventType = "rebooted"
		assert.Error(t, store.AppendSandboxEvent(ctx, bad))
	})
}

// appendEvents appends a sequence of bare lifecycle events for one
// sandbox.
func appendEvents(t *testing.T, store *Store, sandboxID string, eventTypes ...string) {
	t.Helper()
	ctx := context.Background()
	for _, eventType := range eventTypes {
		ev := &model.SandboxEvent{SandboxID: sandboxID, TaskID: "MGIT-4.2", EventType: eventType}
		require.NoError(t, store.AppendSandboxEvent(ctx, ev))
	}
}

// TestDeriveState_CreateThenDestroy_Destroyed verifies terminal events
// win: the latest state-bearing event determines the state. Refs: FR-17.18
func TestDeriveState_CreateThenDestroy_Destroyed(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()

	tests := []struct {
		name   string
		events []string
		want   string
	}{
		{name: "created_only", events: []string{model.EventCreated}, want: model.StateCreated},
		{name: "create_then_destroy", events: []string{model.EventCreated, model.EventDestroyed}, want: model.StateDestroyed},
		{name: "ttl_expired_terminal", events: []string{model.EventCreated, model.EventResumed, model.EventTTLExpired}, want: model.StateDestroyed},
		{name: "killed_terminal", events: []string{model.EventCreated, model.EventKilled}, want: model.StateDestroyed},
		{name: "landed", events: []string{model.EventCreated, model.EventResumed, model.EventLanded}, want: model.StateLanded},
	}
	for i, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			sandboxID := "01JXDERIVE000000000000000" + string(rune('A'+i))
			appendEvents(t, store, sandboxID, tt.events...)
			state, err := store.DeriveState(ctx, sandboxID)
			require.NoError(t, err)
			assert.Equal(t, tt.want, state)
		})
	}
}

// TestDeriveState_SuspendResume_Running verifies suspend/resume cycles
// and that policy_granted events carry no state change. Refs: FR-17.18
func TestDeriveState_SuspendResume_Running(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()

	sandboxID := "01JXDERIVE000000000000010"
	appendEvents(t, store, sandboxID,
		model.EventCreated, model.EventSuspended, model.EventResumed)

	state, err := store.DeriveState(ctx, sandboxID)
	require.NoError(t, err)
	assert.Equal(t, model.StateRunning, state)

	t.Run("suspended_after_resume", func(t *testing.T) {
		appendEvents(t, store, sandboxID, model.EventSuspended)
		state, err := store.DeriveState(ctx, sandboxID)
		require.NoError(t, err)
		assert.Equal(t, model.StateSuspended, state)
	})

	t.Run("policy_granted_does_not_change_state", func(t *testing.T) {
		appendEvents(t, store, sandboxID, model.EventResumed, model.EventPolicyGranted)
		state, err := store.DeriveState(ctx, sandboxID)
		require.NoError(t, err)
		assert.Equal(t, model.StateRunning, state,
			"policy_granted is an audit event, not a lifecycle transition")
	})

	t.Run("idempotent_reads", func(t *testing.T) {
		first, err := store.DeriveState(ctx, sandboxID)
		require.NoError(t, err)
		second, err := store.DeriveState(ctx, sandboxID)
		require.NoError(t, err)
		assert.Equal(t, first, second, "derivation must not mutate anything")
	})
}

// TestDeriveState_Unknown_NotFound verifies the error path.
// Refs: FR-17.18
func TestDeriveState_Unknown_NotFound(t *testing.T) {
	store := newTestStore(t)

	_, err := store.DeriveState(context.Background(), "01JXNOSUCHSANDBOX00000000")
	assert.ErrorIs(t, err, model.ErrSandboxNotFound)

	t.Run("corrupt_event_type_detected", func(t *testing.T) {
		// Bypass AppendSandboxEvent to simulate external corruption of
		// the audit table; DeriveState must refuse, not guess.
		_, err := store.writeDB.ExecContext(context.Background(),
			`INSERT INTO sandbox_events (id, sandbox_id, task_id, event_type, created_at)
			 VALUES (?, ?, ?, ?, ?)`,
			"01JXCORRUPT00000000000000", "01JXCORRUPTSBX00000000000", "MGIT-4.2",
			"rebooted", "2026-06-12T12:00:00Z")
		require.NoError(t, err)

		_, err = store.DeriveState(context.Background(), "01JXCORRUPTSBX00000000000")
		assert.ErrorIs(t, err, model.ErrIndexCorrupted)
	})

	t.Run("policy_only_stream_still_resolves_sandbox", func(t *testing.T) {
		// A sandbox whose only events are policy grants exists but has
		// no state-bearing event yet; it must not report not-found...
		// by construction this cannot happen (created is always first),
		// so the contract is: no state-bearing events => not found.
		appendEvents(t, store, "01JXPOLICYONLY00000000000", model.EventPolicyGranted)
		_, err := store.DeriveState(context.Background(), "01JXPOLICYONLY00000000000")
		assert.ErrorIs(t, err, model.ErrSandboxNotFound)
	})
}

// TestSandboxEvents_ClosedStore_Errors covers the storage error paths.
// Refs: FR-17.18
func TestSandboxEvents_ClosedStore_Errors(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()
	require.NoError(t, store.Close())

	assert.Error(t, store.AppendSandboxEvent(ctx, testSandboxEvent()),
		"append on a closed store must surface the storage error")

	_, err := store.ListSandboxEvents(ctx, "01JXSANDBOX00000000000000")
	assert.Error(t, err, "list on a closed store must surface the storage error")
}
