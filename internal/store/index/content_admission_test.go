package index

import (
	"context"
	"database/sql"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// Refused and policy-off imports keep their raw audit rows, but cannot become
// content authority merely because sandbox_id is NULL or the agent looks local.
func TestContentAdmission_PendingAndUnknownFailClosed(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	require.NoError(t, s.AddCommitToTask(ctx, "MGIT-323", "host", "host-content", "same-agent", 1))
	require.NoError(t, s.AppendTaskCommits(ctx, []TaskCommitInsert{{TaskID: "MGIT-323", CommitHash: "guest", ContentHash: "attested", AgentID: "same-agent", Position: 0, Origin: OriginLand, GuestContentHash: "guest-claim"}}))
	got, err := s.GetContentCommits(ctx, "MGIT-323")
	require.NoError(t, err)
	require.Len(t, got, 1)
	require.Equal(t, "host", got[0].CommitHash)
	raw, err := s.GetTaskCommits(ctx, "MGIT-323")
	require.NoError(t, err)
	require.Len(t, raw, 2)
	require.Nil(t, raw[0].SandboxID)
	require.ErrorContains(t, s.RequireContentCommit(ctx, "guest"), "undelivered")
	require.NoError(t, s.RequireContentCommit(ctx, "host"))
	require.NoError(t, s.AppendTaskCommit(ctx, TaskCommitInsert{TaskID: "MGIT-323", CommitHash: "legacy", Position: 2}))
	_, err = s.GetContentCommits(ctx, "MGIT-323")
	require.ErrorContains(t, err, "ambiguous content eligibility")
	require.ErrorContains(t, err, "legacy")
	require.ErrorContains(t, s.RequireContentCommit(ctx, "missing"), "not an admitted task commit")
}

func TestContentAdmission_DeliveryIsAppendOnlyAndBoundToRow(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	require.NoError(t, s.AppendTaskCommits(ctx, []TaskCommitInsert{{TaskID: "MGIT-323", CommitHash: "guest", ContentHash: "attested", Origin: OriginLand, GuestContentHash: "guest-claim"}}))
	raw, err := s.GetTaskCommits(ctx, "MGIT-323")
	require.NoError(t, err)
	before := raw[0]
	_, err = s.GetContentCommits(ctx, "MGIT-323")
	require.ErrorContains(t, err, "no delivered content")
	wrong := before
	wrong.ContentHash = "guest-claim"
	require.Error(t, s.RecordDelivered(ctx, []CommitRecord{wrong}))
	require.Error(t, s.RequireContentCommit(ctx, "guest"))
	require.NoError(t, s.RecordDelivered(ctx, []CommitRecord{before}))
	require.NoError(t, s.RecordDelivered(ctx, []CommitRecord{before}))
	require.NoError(t, s.RequireContentCommit(ctx, "guest"))
	after, err := s.GetTaskCommits(ctx, "MGIT-323")
	require.NoError(t, err)
	require.Equal(t, raw, after, "delivery must not rewrite the audit ledger")
	require.NoError(t, s.ReadTx(ctx, func(tx *sql.Tx) error {
		var n int
		var guestHash string
		require.NoError(t, tx.QueryRowContext(ctx, "SELECT COUNT(*), MIN(guest_content_hash) FROM content_admissions WHERE task_commit_id = ?", before.ID).Scan(&n, &guestHash))
		require.Equal(t, 2, n, "retry must not duplicate delivery events")
		require.Equal(t, "guest-claim", guestHash)
		return nil
	}))
	for _, statement := range []string{"UPDATE content_admissions SET state = 'delivered'", "DELETE FROM content_admissions"} {
		err := s.WriteTx(ctx, func(tx *sql.Tx) error { _, err := tx.ExecContext(ctx, statement); return err })
		require.ErrorContains(t, err, "append-only")
	}
}

func TestContentAdmission_CannotPromoteLegacyOrImportedRows(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	require.NoError(t, s.AppendTaskCommit(ctx, TaskCommitInsert{TaskID: "MGIT-323", CommitHash: "squash-before-upgrade"}))
	rows, err := s.GetTaskCommits(ctx, "MGIT-323")
	require.NoError(t, err)
	require.ErrorContains(t, s.RecordDelivered(ctx, rows), "ambiguous content eligibility")
	require.ErrorContains(t, s.RequireContentCommit(ctx, rows[0].CommitHash), "ambiguous content eligibility")
}

// A concurrent notifier cannot combine old ledger rows with a newer admission
// decision halfway through a content read. WAL permits the concurrent writer.
func TestContentAdmission_OneReadSnapshot(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	require.NoError(t, s.AppendTaskCommits(ctx, []TaskCommitInsert{{TaskID: "MGIT-323", CommitHash: "guest", ContentHash: "attested", Origin: OriginLand}}))
	raw, err := s.GetTaskCommits(ctx, "MGIT-323")
	require.NoError(t, err)
	require.NoError(t, s.ReadTx(ctx, func(tx *sql.Tx) error {
		before, err := readContentSnapshot(ctx, tx, " WHERE t.task_id = ?", "MGIT-323")
		require.NoError(t, err)
		require.Equal(t, AdmissionPending, before[0].State)
		require.NoError(t, s.RecordDelivered(ctx, raw))
		after, err := readContentSnapshot(ctx, tx, " WHERE t.task_id = ?", "MGIT-323")
		require.NoError(t, err)
		require.Equal(t, before, after, "one reader must not cross the delivery transaction")
		return nil
	}))
	require.NoError(t, s.RequireContentCommit(ctx, "guest"))
}

func TestContentAdmission_LegacyMigrationDoesNotBlessTaintedSquash(t *testing.T) {
	path := filepath.Join(t.TempDir(), "index.db")
	db, err := sql.Open("sqlite", path)
	require.NoError(t, err)
	// A real pre-admission schema, not a new-schema row labeled as legacy.
	_, err = db.Exec(`CREATE TABLE schema_version(version INTEGER NOT NULL, applied_at TEXT NOT NULL);
 INSERT INTO schema_version VALUES(4, '2026-01-01T00:00:00Z');
 CREATE TABLE task_commits(id INTEGER PRIMARY KEY AUTOINCREMENT, task_id TEXT NOT NULL,
 commit_hash TEXT NOT NULL, content_hash TEXT NOT NULL DEFAULT '', agent_id TEXT NOT NULL DEFAULT '',
 position INTEGER NOT NULL DEFAULT 0, created_at TEXT NOT NULL, sandbox_id TEXT, UNIQUE(task_id,commit_hash));
 INSERT INTO task_commits(task_id,commit_hash,content_hash,agent_id,position,created_at)
 VALUES('MGIT-323','legacy-refused','old-content','local-looking',0,'2026-01-01T00:00:00Z'),
 ('MGIT-323','pre-fix-squash','tainted-content','mgit-squash',1,'2026-01-01T00:00:00Z');`)
	require.NoError(t, err)
	require.NoError(t, db.Close())
	s, err := New(path, time.Now)
	require.NoError(t, err)
	t.Cleanup(func() { _ = s.Close() })
	ctx := context.Background()
	version, err := s.SchemaVersion(ctx)
	require.NoError(t, err)
	require.Equal(t, 5, version)
	records, err := s.GetTaskCommits(ctx, "MGIT-323")
	require.NoError(t, err)
	require.Len(t, records, 2)
	_, err = s.GetContentCommits(ctx, "MGIT-323")
	require.ErrorContains(t, err, "ambiguous content eligibility")
	require.ErrorContains(t, s.RequireContentCommit(ctx, "pre-fix-squash"), "ambiguous content eligibility")
	require.ErrorContains(t, s.RecordDelivered(ctx, records), "ambiguous content eligibility")
	snapshot, err := s.GetContentSnapshot(ctx, "MGIT-323")
	require.NoError(t, err)
	for _, r := range snapshot {
		require.Empty(t, r.Origin)
		require.Empty(t, r.State)
	}
}

func TestContentAdmission_UnindexedIsDistinctFromAmbiguous(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	var unindexed *UnindexedContentCommitError
	err := s.RequireContentCommit(ctx, "base-snapshot")
	require.True(t, errors.As(err, &unindexed))
	require.Equal(t, "base-snapshot", unindexed.Hash)
	// No-row hashes are not legacy/imported rows and need no repository migration.
	require.ErrorContains(t, err, "not an admitted task commit")
	require.ErrorContains(t, err, "locally admitted task commit hash")
	require.NotContains(t, err.Error(), "RECOVERY-1")
	require.NotContains(t, err.Error(), "new empty directory")
	require.NoError(t, s.AppendTaskCommit(ctx, TaskCommitInsert{TaskID: "OLD-1", CommitHash: "legacy"}))
	err = s.RequireContentCommit(ctx, "legacy")
	require.ErrorContains(t, err, "ambiguous content eligibility")
	require.False(t, errors.As(err, &unindexed))
	require.NoError(t, s.AppendTaskCommit(ctx, TaskCommitInsert{TaskID: "WAIT-1", CommitHash: "pending", Origin: OriginLand}))
	err = s.RequireContentCommit(ctx, "pending")
	require.ErrorContains(t, err, "undelivered")
	require.False(t, errors.As(err, &unindexed))
}
