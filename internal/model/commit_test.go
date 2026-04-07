package model

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func makeValidCommit(t *testing.T) Commit {
	t.Helper()
	taskID, err := ParseTaskID("MGIT-1.2.3")
	require.NoError(t, err)
	return Commit{
		CommitID:    "abc123def456",
		TaskID:      taskID,
		AgentID:     "agent-01",
		SessionID:   "session-001",
		CreatedAt:   time.Date(2026, 4, 7, 12, 0, 0, 0, time.UTC),
		Message:     "[MGIT:MGIT-1.2.3] implement commit model",
		ParentID:    "",
		TreeHash:    "treehash123",
		FileDiffs:   []FileDiff{{Path: "commit.go", Operation: DiffAdded, NewHash: "abc"}},
		Metadata:    map[string]any{"key": "value"},
		ContentHash: "sha256hash",
		Signature:   "",
		CommitType:  CommitTypeNormal,
		CreatedBy:   "agent-01",
		Branch:      "task/MGIT-1.2",
	}
}

func TestCommit_AllFieldsPresent(t *testing.T) {
	c := makeValidCommit(t)

	// Verify all 16 fields are populated
	assert.NotEmpty(t, c.CommitID)
	assert.False(t, c.TaskID.IsZero())
	assert.NotEmpty(t, c.AgentID)
	assert.NotEmpty(t, c.SessionID)
	assert.False(t, c.CreatedAt.IsZero())
	assert.NotEmpty(t, c.Message)
	// ParentID can be empty for first commit
	assert.NotEmpty(t, c.TreeHash)
	assert.NotEmpty(t, c.FileDiffs)
	assert.NotNil(t, c.Metadata)
	assert.NotEmpty(t, c.ContentHash)
	// Signature can be empty
	assert.NotEmpty(t, c.CommitType)
	assert.NotEmpty(t, c.CreatedBy)
	assert.NotEmpty(t, c.Branch)
}

func TestCommit_JSONMarshal(t *testing.T) {
	c := makeValidCommit(t)

	data, err := json.Marshal(c)
	require.NoError(t, err)

	// Verify JSON tags are correct (snake_case per CLAUDE.md)
	jsonStr := string(data)
	assert.Contains(t, jsonStr, `"commit_id"`)
	assert.Contains(t, jsonStr, `"task_id"`)
	assert.Contains(t, jsonStr, `"agent_id"`)
	assert.Contains(t, jsonStr, `"session_id"`)
	assert.Contains(t, jsonStr, `"created_at"`)
	// parent_id is omitempty, only present when non-empty
	assert.Contains(t, jsonStr, `"tree_hash"`)
	assert.Contains(t, jsonStr, `"file_diffs"`)
	assert.Contains(t, jsonStr, `"content_hash"`)
	assert.Contains(t, jsonStr, `"commit_type"`)
	assert.Contains(t, jsonStr, `"created_by"`)
}

func TestCommit_JSONUnmarshal(t *testing.T) {
	original := makeValidCommit(t)
	data, err := json.Marshal(original)
	require.NoError(t, err)

	var restored Commit
	err = json.Unmarshal(data, &restored)
	require.NoError(t, err)

	assert.Equal(t, original.CommitID, restored.CommitID)
	assert.Equal(t, original.TaskID, restored.TaskID)
	assert.Equal(t, original.AgentID, restored.AgentID)
	assert.Equal(t, original.Message, restored.Message)
	assert.Equal(t, original.ContentHash, restored.ContentHash)
	assert.Equal(t, original.Branch, restored.Branch)
	assert.Len(t, restored.FileDiffs, len(original.FileDiffs))
}

func TestCommit_Validate(t *testing.T) {
	tests := []struct {
		name    string
		modify  func(*Commit)
		wantErr bool
	}{
		{
			name:    "valid_commit",
			modify:  func(_ *Commit) {},
			wantErr: false,
		},
		{
			name:    "missing_commit_id",
			modify:  func(c *Commit) { c.CommitID = "" },
			wantErr: true,
		},
		{
			name:    "missing_task_id",
			modify:  func(c *Commit) { c.TaskID = TaskID{} },
			wantErr: true,
		},
		{
			name:    "zero_timestamp",
			modify:  func(c *Commit) { c.CreatedAt = time.Time{} },
			wantErr: true,
		},
		{
			name:    "missing_message",
			modify:  func(c *Commit) { c.Message = "" },
			wantErr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c := makeValidCommit(t)
			tt.modify(&c)
			err := c.Validate()
			if tt.wantErr {
				assert.Error(t, err)
			} else {
				assert.NoError(t, err)
			}
		})
	}
}

func TestCommit_ValidateFailsOnMissingID(t *testing.T) {
	c := makeValidCommit(t)
	c.CommitID = ""
	err := c.Validate()
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "commit_id")
}

func TestCommit_String(t *testing.T) {
	c := makeValidCommit(t)
	s := c.String()
	assert.Contains(t, s, c.CommitID[:8])
	assert.Contains(t, s, c.TaskID.String())
}

func TestCommit_ShortID(t *testing.T) {
	c := makeValidCommit(t)
	assert.Equal(t, "abc123de", c.ShortID())
}

func TestCommit_CommitTypes(t *testing.T) {
	types := []CommitType{
		CommitTypeNormal, CommitTypeRollback, CommitTypeSquash,
		CommitTypeMerge, CommitTypeSystem,
	}
	for _, ct := range types {
		t.Run(string(ct), func(t *testing.T) {
			assert.NotEmpty(t, string(ct))
		})
	}
}
