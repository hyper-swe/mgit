// Package index tests verify the append-only sandbox_egress_log per
// MGIT-11.3.5 acceptance criteria. Refs: FR-17.8, FR-17.18, NFR-3.1
package index

import (
	"context"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"unicode"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/hyper-swe/mgit/internal/model"
)

func testEgressRecord() *model.EgressRecord {
	return &model.EgressRecord{
		SandboxID: "01JXSANDBOX00000000000000",
		TaskID:    "MGIT-4.2",
		Decision:  model.EgressAllow,
		Protocol:  "tcp",
		DestHost:  "proxy.golang.org",
		DestIP:    "142.250.4.141",
		DestPort:  443,
		Rule:      "proxy.golang.org",
	}
}

// TestSchema_SandboxEgressLogTable verifies the table shape per the
// FR-17.18 egress-log requirement. Refs: FR-17.8, FR-17.18
func TestSchema_SandboxEgressLogTable(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()

	var columns []string
	rows, err := store.readDB.QueryContext(ctx, "PRAGMA table_info(sandbox_egress_log)")
	require.NoError(t, err)
	defer rows.Close() //nolint:errcheck // non-critical
	for rows.Next() {
		var cid, notNull, pk int
		var name, colType string
		var dflt any
		require.NoError(t, rows.Scan(&cid, &name, &colType, &notNull, &dflt, &pk))
		columns = append(columns, name)
	}
	require.NoError(t, rows.Err())

	want := []string{
		"id", "sandbox_id", "task_id", "decision", "protocol",
		"dest_host", "dest_ip", "dest_port", "rule", "created_at",
	}
	assert.ElementsMatch(t, want, columns,
		"sandbox_egress_log must have exactly the specified columns")
}

// TestEgressLog_AppendOnly_NoUpdatePath verifies no UPDATE/DELETE path
// exists for the egress log, in SQL or on the Store surface.
// Refs: FR-17.18, NFR-3.1
func TestEgressLog_AppendOnly_NoUpdatePath(t *testing.T) {
	entries, err := os.ReadDir(".")
	require.NoError(t, err)

	mutationRe := regexp.MustCompile(`(?i)\b(update\s+sandbox_egress_log|delete\s+from\s+sandbox_egress_log)\b`)
	for _, entry := range entries {
		if !strings.HasSuffix(entry.Name(), ".go") || strings.HasSuffix(entry.Name(), "_test.go") {
			continue
		}
		src, err := os.ReadFile(filepath.Clean(entry.Name()))
		require.NoError(t, err)
		assert.False(t, mutationRe.Match(src),
			"%s must not contain UPDATE/DELETE touching sandbox_egress_log", entry.Name())
	}
}

// TestEgressLog_GuestStringsSanitized verifies SEC-04/SEC-07/F-09:
// guest-influenced hostnames and rule strings are control-char-stripped
// and length-capped before entering the append-only log.
func TestEgressLog_GuestStringsSanitized(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()

	rec := testEgressRecord()
	rec.Decision = model.EgressDeny
	// A hostile but within-cap DestHost (control chars + ANSI escape): the model
	// boundary rejects an OVER-cap host outright (see the over-cap assertion
	// below), so the store's job here is control-char stripping. The Rule is
	// deliberately oversized to exercise the store's truncation.
	rec.DestHost = "evil\nFAKE-ROW\x1b[31m." + strings.Repeat("a", 100) + ".example"
	rec.Rule = "denied: unresolvable SNI \x00" + strings.Repeat("b", 9000)
	require.NoError(t, store.AppendEgressRecord(ctx, rec))

	// An OVER-cap DestHost is rejected at the model boundary before it can reach
	// the store at all (defense-in-depth, F-09).
	oversized := testEgressRecord()
	oversized.DestHost = strings.Repeat("a", model.MaxEgressDestHostLen+1)
	require.Error(t, store.AppendEgressRecord(ctx, oversized),
		"an over-cap dest_host is rejected before entering the append-only log")

	got, err := store.ListEgressBySandbox(ctx, rec.SandboxID)
	require.NoError(t, err)
	require.Len(t, got, 1)

	for name, value := range map[string]string{"dest_host": got[0].DestHost, "rule": got[0].Rule} {
		assert.False(t, strings.ContainsFunc(value, unicode.IsControl),
			"%s must contain no control characters, got %q", name, value)
	}
	assert.LessOrEqual(t, len(got[0].DestHost), maxEgressHostLen, "dest_host length-capped")
	assert.LessOrEqual(t, len(got[0].Rule), maxSandboxEventDetailLen, "rule length-capped")
}

// TestEgressLog_QueryBySandbox verifies audit queries by sandbox and
// task, append ordering, and the error paths. Refs: FR-17.18
func TestEgressLog_QueryBySandbox(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()

	allow := testEgressRecord()
	require.NoError(t, store.AppendEgressRecord(ctx, allow))

	deny := testEgressRecord()
	deny.Decision = model.EgressDeny
	deny.DestHost = "169.254.169.254"
	deny.DestIP = "169.254.169.254"
	deny.DestPort = 80
	deny.Rule = "denied: metadata endpoint"
	require.NoError(t, store.AppendEgressRecord(ctx, deny))

	other := testEgressRecord()
	other.SandboxID = "01JXOTHERSANDBOX000000000"
	other.TaskID = "MGIT-5.1"
	require.NoError(t, store.AppendEgressRecord(ctx, other))

	bySandbox, err := store.ListEgressBySandbox(ctx, allow.SandboxID)
	require.NoError(t, err)
	require.Len(t, bySandbox, 2, "only the addressed sandbox's records")
	assert.Equal(t, model.EgressAllow, bySandbox[0].Decision)
	assert.Equal(t, model.EgressDeny, bySandbox[1].Decision)
	assert.Less(t, bySandbox[0].ID, bySandbox[1].ID, "append order via ULID ids")
	assert.False(t, bySandbox[0].CreatedAt.IsZero())

	byTask, err := store.ListEgressByTask(ctx, "MGIT-5.1")
	require.NoError(t, err)
	require.Len(t, byTask, 1)
	assert.Equal(t, other.SandboxID, byTask[0].SandboxID)

	t.Run("unknown_sandbox_empty", func(t *testing.T) {
		got, err := store.ListEgressBySandbox(ctx, "01JXNOSUCH000000000000000")
		require.NoError(t, err)
		assert.Empty(t, got)
	})

	t.Run("invalid_record_rejected", func(t *testing.T) {
		bad := testEgressRecord()
		bad.Decision = "maybe"
		assert.Error(t, store.AppendEgressRecord(ctx, bad))
	})

	t.Run("closed_store_errors", func(t *testing.T) {
		closed := newTestStore(t)
		require.NoError(t, closed.Close())
		assert.Error(t, closed.AppendEgressRecord(ctx, testEgressRecord()))
		_, err := closed.ListEgressBySandbox(ctx, allow.SandboxID)
		assert.Error(t, err)
		_, err = closed.ListEgressByTask(ctx, "MGIT-4.2")
		assert.Error(t, err)
	})
}
