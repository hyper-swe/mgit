package controlproto

import (
	"bytes"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/hyper-swe/mgit/internal/model"
)

// TestControlProto_SyncRequestRoundTrip verifies the sync verb's arguments
// survive the wire: a lost DryRun would turn a query into a delivery, and a
// lost Force would turn a refusal into an overwrite. Refs: MGIT-76
func TestControlProto_SyncRequestRoundTrip(t *testing.T) {
	for name, args := range map[string]*SyncArgs{
		"plain":   {TaskID: "MGIT-76"},
		"force":   {TaskID: "MGIT-76", Sync: model.WorktreeSyncOptions{Force: true}},
		"dry_run": {TaskID: "MGIT-76", Sync: model.WorktreeSyncOptions{DryRun: true}},
		"both":    {TaskID: "MGIT-76", Sync: model.WorktreeSyncOptions{Force: true, DryRun: true}},
	} {
		t.Run(name, func(t *testing.T) {
			var buf bytes.Buffer
			require.NoError(t, WriteRequest(&buf, &Request{Kind: KindSync, Sync: args}))

			got, err := ReadRequest(&buf)

			require.NoError(t, err)
			assert.Equal(t, KindSync, got.Kind)
			assert.Equal(t, args, got.Sync)
		})
	}
}

// TestControlProto_SyncRequestMissingTask_Rejected verifies the sync verb
// fails closed on a payload that names no sandbox.
func TestControlProto_SyncRequestMissingTask_Rejected(t *testing.T) {
	for name, req := range map[string]*Request{
		"no_payload": {Kind: KindSync},
		"no_task":    {Kind: KindSync, Sync: &SyncArgs{}},
	} {
		t.Run(name, func(t *testing.T) {
			var buf bytes.Buffer
			require.NoError(t, WriteRequest(&buf, req))

			_, err := ReadRequest(&buf)

			require.Error(t, err)
		})
	}
}

// TestControlProto_SyncResponseCarriesTheClassification verifies the whole
// report crosses the wire, including the conflicts — that report is the
// capability MGIT-76 adds, and an error string alone would not carry it.
func TestControlProto_SyncResponseCarriesTheClassification(t *testing.T) {
	want := &model.WorktreeSyncReport{
		Updated:   []string{"app.go"},
		Deleted:   []string{"old.go"},
		Conflicts: []model.WorktreeSyncConflict{{Path: "lib.go", Reason: "modified in the guest"}},
		DryRun:    true, Refused: true,
	}
	var buf bytes.Buffer
	require.NoError(t, WriteResponse(&buf, &Response{Synced: want}))

	got, err := ReadResponse(&buf)

	require.NoError(t, err)
	assert.Equal(t, want, got.Synced)
}

// TestControlProto_SyncResponseCarriesReportAndErrorTogether verifies a
// REFUSAL can carry both: the error the caller must not ignore, and the
// classification that names what was refused. Refs: MGIT-76
func TestControlProto_SyncResponseCarriesReportAndErrorTogether(t *testing.T) {
	var buf bytes.Buffer
	require.NoError(t, WriteResponse(&buf, &Response{
		Error:  "sandbox sync: blocked",
		Synced: &model.WorktreeSyncReport{Refused: true, Conflicts: []model.WorktreeSyncConflict{{Path: "app.go"}}},
	}))

	got, err := ReadResponse(&buf)

	require.NoError(t, err)
	assert.Equal(t, "sandbox sync: blocked", got.Error)
	require.NotNil(t, got.Synced)
	assert.Equal(t, "app.go", got.Synced.Conflicts[0].Path)
}

// TestControlProto_SyncPayloadOnWrongKind_Rejected keeps the kind/payload
// coupling closed for the new verb too.
func TestControlProto_SyncPayloadOnWrongKind_Rejected(t *testing.T) {
	var buf bytes.Buffer
	require.NoError(t, WriteRequest(&buf, &Request{
		Kind: KindStatus, Status: &TaskRef{TaskID: "MGIT-76"}, Sync: &SyncArgs{TaskID: "MGIT-76"},
	}))

	_, err := ReadRequest(&buf)

	require.Error(t, err)
	assert.Contains(t, err.Error(), "payload")
}
