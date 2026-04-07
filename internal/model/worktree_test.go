package model

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestWorktreeInfo_JSONSerialization(t *testing.T) {
	wt := WorktreeInfo{
		Path:      "/tmp/worktrees/agent-01",
		Name:      "agent-01",
		Branch:    "task/MGIT-1.2",
		TaskID:    "MGIT-1.2",
		AgentID:   "agent-01",
		CreatedAt: time.Date(2026, 4, 7, 12, 0, 0, 0, time.UTC),
	}

	data, err := json.Marshal(wt)
	require.NoError(t, err)

	var restored WorktreeInfo
	require.NoError(t, json.Unmarshal(data, &restored))
	assert.Equal(t, wt.Path, restored.Path)
	assert.Equal(t, wt.TaskID, restored.TaskID)
}

func TestWorktreeInfo_Validate_ValidInput(t *testing.T) {
	wt := WorktreeInfo{Path: "/tmp/wt", TaskID: "MGIT-1.2"}
	assert.NoError(t, wt.Validate())
}

func TestWorktreeInfo_Validate_EmptyPath(t *testing.T) {
	wt := WorktreeInfo{Path: "", TaskID: "MGIT-1.2"}
	assert.Error(t, wt.Validate())
}

func TestWorktreeInfo_Validate_InvalidTaskID(t *testing.T) {
	wt := WorktreeInfo{Path: "/tmp/wt", TaskID: "invalid"}
	assert.Error(t, wt.Validate())
}

func TestWorktreeAddOptions_Validate(t *testing.T) {
	tests := []struct {
		name    string
		opts    WorktreeAddOptions
		wantErr bool
	}{
		{"valid", WorktreeAddOptions{Path: "/tmp/wt", TaskID: "MGIT-1.2"}, false},
		{"empty_path", WorktreeAddOptions{Path: "", TaskID: "MGIT-1.2"}, true},
		{"empty_task", WorktreeAddOptions{Path: "/tmp/wt", TaskID: ""}, true},
		{"invalid_task", WorktreeAddOptions{Path: "/tmp/wt", TaskID: "bad"}, true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := tt.opts.Validate()
			if tt.wantErr {
				assert.Error(t, err)
			} else {
				assert.NoError(t, err)
			}
		})
	}
}

func TestDeriveNameFromPath(t *testing.T) {
	assert.Equal(t, "agent-01", DeriveNameFromPath("/tmp/worktrees/agent-01"))
	assert.Equal(t, "mywork", DeriveNameFromPath("/home/user/mywork"))
}
