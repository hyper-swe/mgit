package service

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/hyper-swe/mgit/internal/model"
	gitstore "github.com/hyper-swe/mgit/internal/store/git"
)

// --- foldDiffs + inverseFromStates: net-and-invert (MGIT-54) ---

func TestFoldAndInvert_AllOperations(t *testing.T) {
	tests := []struct {
		name    string
		input   []model.FileDiff
		wantLen int
		wantOps []model.DiffOperation
	}{
		{
			name:    "empty",
			input:   nil,
			wantLen: 0,
		},
		{
			name: "added_inverts_to_deleted",
			input: []model.FileDiff{
				{Path: "new.go", Operation: model.DiffAdded, NewHash: "abc"},
			},
			wantLen: 1,
			wantOps: []model.DiffOperation{model.DiffDeleted},
		},
		{
			name: "deleted_inverts_to_added",
			input: []model.FileDiff{
				{Path: "old.go", Operation: model.DiffDeleted, OldHash: "def"},
			},
			wantLen: 1,
			wantOps: []model.DiffOperation{model.DiffAdded},
		},
		{
			name: "modified_swaps_hashes",
			input: []model.FileDiff{
				{Path: "mod.go", Operation: model.DiffModified, OldHash: "a", NewHash: "b"},
			},
			wantLen: 1,
			wantOps: []model.DiffOperation{model.DiffModified},
		},
		{
			name: "add_then_delete_nets_to_nothing",
			input: []model.FileDiff{
				{Path: "tmp.go", Operation: model.DiffAdded, NewHash: "x"},
				{Path: "tmp.go", Operation: model.DiffDeleted, OldHash: "x"},
			},
			wantLen: 0,
		},
		{
			name: "modify_back_to_original_nets_to_nothing",
			input: []model.FileDiff{
				{Path: "a.go", Operation: model.DiffModified, OldHash: "v1", NewHash: "v2"},
				{Path: "a.go", Operation: model.DiffModified, OldHash: "v2", NewHash: "v1"},
			},
			wantLen: 0,
		},
		{
			name: "chain_of_modifies_nets_to_single_inverse",
			input: []model.FileDiff{
				{Path: "a.go", Operation: model.DiffModified, OldHash: "v1", NewHash: "v2"},
				{Path: "a.go", Operation: model.DiffModified, OldHash: "v2", NewHash: "v3"},
			},
			wantLen: 1,
			wantOps: []model.DiffOperation{model.DiffModified},
		},
		{
			name: "multiple_mixed_sorted_by_path",
			input: []model.FileDiff{
				{Path: "a.go", Operation: model.DiffAdded, NewHash: "1"},
				{Path: "b.go", Operation: model.DiffDeleted, OldHash: "2"},
				{Path: "c.go", Operation: model.DiffModified, OldHash: "3", NewHash: "4"},
			},
			wantLen: 3,
			wantOps: []model.DiffOperation{model.DiffDeleted, model.DiffAdded, model.DiffModified},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			states := map[string]*pathState{}
			require.NoError(t, foldDiffs(states, tt.input))
			result := inverseFromStates(states)
			assert.Len(t, result, tt.wantLen)
			for i, wantOp := range tt.wantOps {
				assert.Equal(t, wantOp, result[i].Operation)
			}
		})
	}
}

// The inverse of a net modification restores the ORIGINAL hash. Refs: MGIT-54
func TestInverseFromStates_ModifiedRestoresOriginalHash(t *testing.T) {
	states := map[string]*pathState{}
	require.NoError(t, foldDiffs(states, []model.FileDiff{
		{Path: "a.go", Operation: model.DiffModified, OldHash: "orig", NewHash: "mid"},
		{Path: "a.go", Operation: model.DiffModified, OldHash: "mid", NewHash: "last"},
	}))
	inv := inverseFromStates(states)
	assert.Len(t, inv, 1)
	assert.Equal(t, "last", inv[0].OldHash, "inverse expects the task's final state")
	assert.Equal(t, "orig", inv[0].NewHash, "inverse restores the pre-task state")
}

// --- mergeDiffs: edge cases ---

func TestMergeDiffs_Empty(t *testing.T) {
	result := mergeDiffs(nil)
	assert.Empty(t, result)
}

func TestMergeDiffs_SingleFile(t *testing.T) {
	result := mergeDiffs([]model.FileDiff{
		{Path: "a.go", Operation: model.DiffAdded, NewHash: "h1"},
	})
	assert.Len(t, result, 1)
	assert.Equal(t, "a.go", result[0].Path)
}

func TestMergeDiffs_LastWriteWins(t *testing.T) {
	result := mergeDiffs([]model.FileDiff{
		{Path: "a.go", Operation: model.DiffAdded, NewHash: "h1"},
		{Path: "a.go", Operation: model.DiffModified, NewHash: "h2"},
		{Path: "a.go", Operation: model.DiffModified, NewHash: "h3"},
	})
	assert.Len(t, result, 1)
	assert.Equal(t, "h3", result[0].NewHash)
	assert.Equal(t, model.DiffModified, result[0].Operation)
}

// --- MergeService: divergent branches (non-fast-forward auto) ---

func TestMergeService_Merge_AutoWithDivergedBranches(t *testing.T) {
	env, mergeSvc := setupMergeEnv(t)
	ctx := context.Background()

	// Create a task branch.
	_, err := env.branch.CreateBranch(ctx, "MGIT-16.1")
	require.NoError(t, err)
	err = env.branch.SwitchBranch(ctx, "task/MGIT-16.1")
	require.NoError(t, err)

	_, err = env.commit.CreateCommit(ctx, CreateCommitRequest{
		TaskID: "MGIT-16.1", AgentID: "a", Message: "branch work",
	})
	require.NoError(t, err)

	// Switch back to main and create a commit to diverge.
	err = env.branch.SwitchBranch(ctx, "main")
	require.NoError(t, err)

	_, err = env.commit.CreateCommit(ctx, CreateCommitRequest{
		TaskID: "MGIT-16.2", AgentID: "a", Message: "main work",
	})
	require.NoError(t, err)

	// Auto merge on diverged branches should create a merge commit.
	result, err := mergeSvc.Merge(ctx, MergeRequest{
		SourceBranch: "task/MGIT-16.1",
		Strategy:     MergeAuto,
	})
	require.NoError(t, err)
	assert.False(t, result.FastFwd)
	assert.Equal(t, "merged", result.Status)
}

// --- CheckoutService: dirty worktree ---

func TestCheckoutService_Checkout_DirtyWorktree(t *testing.T) {
	env, checkoutSvc := setupCheckoutEnv(t)
	ctx := context.Background()

	// Create a branch to try to checkout to.
	_, err := env.branch.CreateBranch(ctx, "MGIT-17.1")
	require.NoError(t, err)

	// Create an unstaged file to make the worktree dirty.
	repoRoot := filepath.Dir(env.repo.MgitDir())
	dirtyFile := filepath.Join(repoRoot, "dirty.txt")
	require.NoError(t, os.WriteFile(dirtyFile, []byte("dirty"), 0o600))

	// Stage it so go-git sees it as a pending change.
	ws := gitstore.NewWorktreeStore(env.repo)
	require.NoError(t, ws.Add(ctx, "dirty.txt"))

	_, err = checkoutSvc.Checkout(ctx, "task/MGIT-17.1")
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "uncommitted changes")
}

// --- FormatUnified: renamed file operation ---

func TestDiffService_FormatUnified_Renamed(t *testing.T) {
	ds := &DiffService{}
	diffs := []model.FileDiff{
		{Path: "renamed.go", Operation: model.DiffRenamed, OldHash: "aaa", NewHash: "bbb"},
	}
	out := ds.FormatUnified(diffs)
	assert.Contains(t, out, "renamed: renamed.go")
}

// --- ConfigService: Set top-level key ---

func TestConfigService_Set_TopLevelKey(t *testing.T) {
	configPath := filepath.Join(t.TempDir(), "config.json")
	svc, err := NewConfigService(configPath)
	require.NoError(t, err)

	// Single-segment key sets at top level of config map.
	err = svc.Set("custom_key", "custom_value")
	assert.NoError(t, err)
}

// --- ConfigService: Get with single segment ---

func TestConfigService_Get_SingleSegment(t *testing.T) {
	configPath := filepath.Join(t.TempDir(), "config.json")
	svc, err := NewConfigService(configPath)
	require.NoError(t, err)

	_, err = svc.Get("single")
	assert.Error(t, err)
}

// --- AuditService: LogOperation with pre-set timestamp ---

func TestAuditService_LogOperation_PresetTimestamp(t *testing.T) {
	logPath := filepath.Join(t.TempDir(), "audit.log")
	svc := NewAuditService(logPath, fixedClock())

	err := svc.LogOperation(AuditEntry{
		Timestamp: "2026-04-07T12:00:00Z",
		Operation: AuditCreateCommit,
		AgentID:   "agent-01",
		TaskID:    "MGIT-1.1",
	})
	require.NoError(t, err)

	entries, err := svc.GetAuditLog(AuditFilters{})
	require.NoError(t, err)
	require.Len(t, entries, 1)
	assert.Equal(t, "2026-04-07T12:00:00Z", entries[0].Timestamp)
}

// --- BundleService: Export with nonexistent task ---

func TestBundleService_Export_NonexistentTask(t *testing.T) {
	env := setupTestEnv(t)
	ctx := context.Background()
	clock := fixedClock()

	bundleSvc := NewBundleService(env.idx, clock)
	// Exporting a task with no commits returns an empty bundle (not an error).
	data, err := bundleSvc.Export(ctx, []string{"MGIT-99.99"})
	require.NoError(t, err)
	assert.NotNil(t, data)
}
