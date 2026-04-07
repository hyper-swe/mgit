package model

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func makeValidBranch(t *testing.T) Branch {
	t.Helper()
	taskID, err := ParseTaskID("MGIT-1.2")
	require.NoError(t, err)
	return Branch{
		BranchID:    "branch-001",
		Name:        "task/MGIT-1.2",
		HeadCommit:  "abc123",
		TaskID:      taskID,
		CreatedAt:   time.Date(2026, 4, 7, 12, 0, 0, 0, time.UTC),
		LockedBy:    "",
		LockedUntil: time.Time{},
		MergedTo:    "",
		IsMerged:    false,
	}
}

func TestBranch_AllFieldsPresent(t *testing.T) {
	b := makeValidBranch(t)
	assert.NotEmpty(t, b.BranchID)
	assert.NotEmpty(t, b.Name)
	assert.NotEmpty(t, b.HeadCommit)
	assert.False(t, b.TaskID.IsZero())
	assert.False(t, b.CreatedAt.IsZero())
	// LockedBy, LockedUntil, MergedTo can be empty
	assert.False(t, b.IsMerged)
}

func TestBranch_Lock(t *testing.T) {
	b := makeValidBranch(t)
	now := time.Date(2026, 4, 7, 12, 0, 0, 0, time.UTC)
	lockDuration := 30 * time.Second

	err := b.Lock("agent-02", now, lockDuration)
	require.NoError(t, err)

	assert.Equal(t, "agent-02", b.LockedBy)
	assert.Equal(t, now.Add(lockDuration), b.LockedUntil)
}

func TestBranch_Lock_AlreadyLocked(t *testing.T) {
	b := makeValidBranch(t)
	now := time.Date(2026, 4, 7, 12, 0, 0, 0, time.UTC)
	lockDuration := 30 * time.Second

	err := b.Lock("agent-01", now, lockDuration)
	require.NoError(t, err)

	// Second lock by different agent should fail if lock not expired
	err = b.Lock("agent-02", now.Add(10*time.Second), lockDuration)
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "locked")
}

func TestBranch_Lock_ExpiredLock(t *testing.T) {
	b := makeValidBranch(t)
	now := time.Date(2026, 4, 7, 12, 0, 0, 0, time.UTC)
	lockDuration := 30 * time.Second

	err := b.Lock("agent-01", now, lockDuration)
	require.NoError(t, err)

	// Lock by different agent should succeed after expiry
	err = b.Lock("agent-02", now.Add(31*time.Second), lockDuration)
	assert.NoError(t, err)
	assert.Equal(t, "agent-02", b.LockedBy)
}

func TestBranch_Unlock(t *testing.T) {
	b := makeValidBranch(t)
	now := time.Date(2026, 4, 7, 12, 0, 0, 0, time.UTC)

	err := b.Lock("agent-01", now, 30*time.Second)
	require.NoError(t, err)

	b.Unlock()
	assert.Empty(t, b.LockedBy)
	assert.True(t, b.LockedUntil.IsZero())
}

func TestBranch_Validate(t *testing.T) {
	tests := []struct {
		name    string
		modify  func(*Branch)
		wantErr bool
	}{
		{
			name:    "valid",
			modify:  func(_ *Branch) {},
			wantErr: false,
		},
		{
			name:    "empty_name",
			modify:  func(b *Branch) { b.Name = "" },
			wantErr: true,
		},
		{
			name:    "zero_task_id",
			modify:  func(b *Branch) { b.TaskID = TaskID{} },
			wantErr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			b := makeValidBranch(t)
			tt.modify(&b)
			err := b.Validate()
			if tt.wantErr {
				assert.Error(t, err)
			} else {
				assert.NoError(t, err)
			}
		})
	}
}

func TestBranch_JSONRoundtrip(t *testing.T) {
	original := makeValidBranch(t)

	data, err := json.Marshal(original)
	require.NoError(t, err)

	var restored Branch
	err = json.Unmarshal(data, &restored)
	require.NoError(t, err)

	assert.Equal(t, original.BranchID, restored.BranchID)
	assert.Equal(t, original.Name, restored.Name)
	assert.Equal(t, original.TaskID, restored.TaskID)
	assert.Equal(t, original.HeadCommit, restored.HeadCommit)
	assert.Equal(t, original.IsMerged, restored.IsMerged)
}

func TestBranch_String(t *testing.T) {
	b := makeValidBranch(t)
	s := b.String()
	assert.Contains(t, s, b.Name)
	assert.Contains(t, s, b.TaskID.String())
}

func TestBranch_IsLocked(t *testing.T) {
	b := makeValidBranch(t)
	now := time.Date(2026, 4, 7, 12, 0, 0, 0, time.UTC)

	assert.False(t, b.IsLocked(now), "unlocked branch should return false")

	err := b.Lock("agent-01", now, 30*time.Second)
	require.NoError(t, err)
	assert.True(t, b.IsLocked(now.Add(10*time.Second)), "locked branch should return true")
	assert.False(t, b.IsLocked(now.Add(31*time.Second)), "expired lock should return false")
}
