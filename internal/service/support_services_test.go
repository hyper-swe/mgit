package service

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/hyper-swe/mgit-dev/internal/model"
	gitstore "github.com/hyper-swe/mgit-dev/internal/store/git"
)

// --- DiffService Tests ---

func TestDiffService_DiffCommits(t *testing.T) {
	env := setupTestEnv(t)
	ctx := context.Background()

	head1, err := env.repo.Head()
	require.NoError(t, err)

	_, err = env.commit.CreateCommit(ctx, CreateCommitRequest{
		TaskID: "MGIT-1.1", AgentID: "agent-01", Message: "change",
	})
	require.NoError(t, err)

	head2, err := env.repo.Head()
	require.NoError(t, err)

	ds := gitstore.NewDiffStore(env.repo)
	diffSvc := NewDiffService(ds, env.cs, env.idx)

	diffs, err := diffSvc.DiffCommits(ctx, head1, head2)
	require.NoError(t, err)
	assert.NotNil(t, diffs)
}

func TestDiffService_DiffTask(t *testing.T) {
	env := setupTestEnv(t)
	ctx := context.Background()

	_, err := env.commit.CreateCommit(ctx, CreateCommitRequest{
		TaskID: "MGIT-2.1", AgentID: "agent-01", Message: "first",
	})
	require.NoError(t, err)

	_, err = env.commit.CreateCommit(ctx, CreateCommitRequest{
		TaskID: "MGIT-2.1", AgentID: "agent-01", Message: "second",
	})
	require.NoError(t, err)

	ds := gitstore.NewDiffStore(env.repo)
	diffSvc := NewDiffService(ds, env.cs, env.idx)

	diffs, err := diffSvc.DiffTask(ctx, "MGIT-2.1")
	require.NoError(t, err)
	assert.NotNil(t, diffs)
}

func TestDiffService_DiffTask_NotFound(t *testing.T) {
	env := setupTestEnv(t)
	ctx := context.Background()

	ds := gitstore.NewDiffStore(env.repo)
	diffSvc := NewDiffService(ds, env.cs, env.idx)

	_, err := diffSvc.DiffTask(ctx, "MGIT-99.9")
	assert.ErrorIs(t, err, model.ErrTaskNotFound)
}

func TestDiffService_Statistics(t *testing.T) {
	ds := &DiffService{}
	diffs := []model.FileDiff{
		{
			Path: "a.go", Operation: model.DiffModified,
			Hunks: []model.Hunk{
				{LinesAdded: 5, LinesRemoved: 2},
				{LinesAdded: 3, LinesRemoved: 1},
			},
		},
	}
	stats := ds.Statistics(diffs)
	assert.Equal(t, 8, stats.LinesAdded)
	assert.Equal(t, 3, stats.LinesRemoved)
}

// --- VerifyService Tests ---

func TestVerifyService_VerifyCommitChain(t *testing.T) {
	env := setupTestEnv(t)
	ctx := context.Background()

	c1, err := env.commit.CreateCommit(ctx, CreateCommitRequest{
		TaskID: "MGIT-3.1", AgentID: "agent-01", Message: "first",
	})
	require.NoError(t, err)

	c2, err := env.commit.CreateCommit(ctx, CreateCommitRequest{
		TaskID: "MGIT-3.1", AgentID: "agent-01", Message: "second",
	})
	require.NoError(t, err)

	verifySvc := NewVerifyService(env.cs, env.idx)
	err = verifySvc.VerifyCommitChain(ctx, []string{c1.CommitID, c2.CommitID})
	assert.NoError(t, err, "valid chain must pass verification")
}

func TestVerifyService_VerifyCommitChain_Empty(t *testing.T) {
	env := setupTestEnv(t)
	ctx := context.Background()

	verifySvc := NewVerifyService(env.cs, env.idx)
	err := verifySvc.VerifyCommitChain(ctx, []string{})
	assert.NoError(t, err, "empty chain must pass")
}

func TestVerifyService_VerifyTaskCommits(t *testing.T) {
	env := setupTestEnv(t)
	ctx := context.Background()

	_, err := env.commit.CreateCommit(ctx, CreateCommitRequest{
		TaskID: "MGIT-4.1", AgentID: "agent-01", Message: "commit",
	})
	require.NoError(t, err)

	verifySvc := NewVerifyService(env.cs, env.idx)
	err = verifySvc.VerifyTaskCommits(ctx, "MGIT-4.1")
	assert.NoError(t, err)
}

func TestVerifyService_VerifyIndexIntegrity(t *testing.T) {
	env := setupTestEnv(t)
	ctx := context.Background()

	verifySvc := NewVerifyService(env.cs, env.idx)
	issues, err := verifySvc.VerifyIndexIntegrity(ctx)
	require.NoError(t, err)
	// Initial commit won't be in index, so there should be issues
	assert.NotNil(t, issues)
}

func TestVerifyService_VerifyIndexIntegrity_DetectsMissing(t *testing.T) {
	env := setupTestEnv(t)
	ctx := context.Background()

	// Create a commit in git but NOT in the index
	c := &model.Commit{
		TaskID:     model.TaskID{},
		AgentID:    "agent-01",
		Message:    "unindexed commit",
		CommitType: model.CommitTypeNormal,
		CreatedBy:  "agent-01",
	}
	// Parse a valid TaskID for the commit
	tid, _ := model.ParseTaskID("MGIT-5.1")
	c.TaskID = tid
	_, err := env.cs.CreateCommit(ctx, c)
	require.NoError(t, err)

	verifySvc := NewVerifyService(env.cs, env.idx)
	issues, err := verifySvc.VerifyIndexIntegrity(ctx)
	require.NoError(t, err)
	assert.NotEmpty(t, issues, "must detect unindexed commits")
}

// --- AuditService Tests ---

func TestAuditService_LogOperation(t *testing.T) {
	logPath := filepath.Join(t.TempDir(), "audit.log")
	svc := NewAuditService(logPath, fixedClock())

	err := svc.LogOperation(AuditEntry{
		Operation: AuditCreateCommit,
		AgentID:   "agent-01",
		TaskID:    "MGIT-1.2.3",
		CommitID:  "abc123",
		Details:   "created commit",
	})
	assert.NoError(t, err)
}

func TestAuditService_GetAuditLog(t *testing.T) {
	logPath := filepath.Join(t.TempDir(), "audit.log")
	svc := NewAuditService(logPath, fixedClock())

	require.NoError(t, svc.LogOperation(AuditEntry{
		Operation: AuditCreateCommit, AgentID: "agent-01", TaskID: "MGIT-1.1",
	}))
	require.NoError(t, svc.LogOperation(AuditEntry{
		Operation: AuditSquash, AgentID: "agent-02", TaskID: "MGIT-2.1",
	}))

	entries, err := svc.GetAuditLog(AuditFilters{})
	require.NoError(t, err)
	assert.Len(t, entries, 2)
}

func TestAuditService_GetAuditLog_WithFilters(t *testing.T) {
	logPath := filepath.Join(t.TempDir(), "audit.log")
	svc := NewAuditService(logPath, fixedClock())

	require.NoError(t, svc.LogOperation(AuditEntry{
		Operation: AuditCreateCommit, AgentID: "agent-01", TaskID: "MGIT-1.1",
	}))
	require.NoError(t, svc.LogOperation(AuditEntry{
		Operation: AuditSquash, AgentID: "agent-02", TaskID: "MGIT-2.1",
	}))
	require.NoError(t, svc.LogOperation(AuditEntry{
		Operation: AuditCreateCommit, AgentID: "agent-01", TaskID: "MGIT-3.1",
	}))

	// Filter by agent
	entries, err := svc.GetAuditLog(AuditFilters{AgentID: "agent-01"})
	require.NoError(t, err)
	assert.Len(t, entries, 2)

	// Filter by operation
	entries, err = svc.GetAuditLog(AuditFilters{Operation: AuditSquash})
	require.NoError(t, err)
	assert.Len(t, entries, 1)

	// Filter by task
	entries, err = svc.GetAuditLog(AuditFilters{TaskID: "MGIT-2.1"})
	require.NoError(t, err)
	assert.Len(t, entries, 1)
}

func TestAuditService_ExportAuditLog(t *testing.T) {
	logPath := filepath.Join(t.TempDir(), "audit.log")
	svc := NewAuditService(logPath, fixedClock())

	require.NoError(t, svc.LogOperation(AuditEntry{
		Operation: AuditCreateCommit, AgentID: "agent-01", TaskID: "MGIT-1.1",
	}))

	data, err := svc.ExportAuditLog(AuditFilters{})
	require.NoError(t, err)
	assert.Contains(t, string(data), "CREATE_COMMIT")
	assert.Contains(t, string(data), "agent-01")
}

func TestAuditService_GetAuditLog_EmptyFile(t *testing.T) {
	logPath := filepath.Join(t.TempDir(), "nonexistent.log")
	svc := NewAuditService(logPath, fixedClock())

	entries, err := svc.GetAuditLog(AuditFilters{})
	require.NoError(t, err)
	assert.Nil(t, entries)
}

// --- ConfigService Tests ---

func TestConfigService_LoadConfig(t *testing.T) {
	configPath := filepath.Join(t.TempDir(), "config.json")
	svc, err := NewConfigService(configPath)
	require.NoError(t, err)
	assert.NotNil(t, svc)
}

func TestConfigService_Get(t *testing.T) {
	configPath := filepath.Join(t.TempDir(), "config.json")
	svc, err := NewConfigService(configPath)
	require.NoError(t, err)

	val, err := svc.Get("api.http_port")
	require.NoError(t, err)
	// JSON numbers unmarshal as float64
	assert.Equal(t, float64(6860), val)
}

func TestConfigService_Get_DotNotation(t *testing.T) {
	configPath := filepath.Join(t.TempDir(), "config.json")
	svc, err := NewConfigService(configPath)
	require.NoError(t, err)

	tests := []struct {
		key  string
		want any
	}{
		{"project.prefix", "MGIT"},
		{"api.bind_address", "127.0.0.1"},
		{"logging.level", "info"},
		{"mcp.transport", "stdio"},
		{"squash.auto_notify", true},
		{"rollback.auto_reopen", true},
		{"branch.auto_create", true},
	}

	for _, tt := range tests {
		t.Run(tt.key, func(t *testing.T) {
			val, err := svc.Get(tt.key)
			require.NoError(t, err)
			assert.Equal(t, tt.want, val)
		})
	}
}

func TestConfigService_Get_NotFound(t *testing.T) {
	configPath := filepath.Join(t.TempDir(), "config.json")
	svc, err := NewConfigService(configPath)
	require.NoError(t, err)

	_, err = svc.Get("nonexistent.key")
	assert.Error(t, err)
}

func TestConfigService_Set(t *testing.T) {
	configPath := filepath.Join(t.TempDir(), "config.json")
	svc, err := NewConfigService(configPath)
	require.NoError(t, err)

	err = svc.Set("logging.level", "debug")
	require.NoError(t, err)

	val, err := svc.Get("logging.level")
	require.NoError(t, err)
	assert.Equal(t, "debug", val)
}

func TestConfigService_SaveConfig(t *testing.T) {
	configPath := filepath.Join(t.TempDir(), "config.json")
	svc, err := NewConfigService(configPath)
	require.NoError(t, err)

	err = svc.Set("project.name", "my-project")
	require.NoError(t, err)

	err = svc.Save()
	require.NoError(t, err)

	// Reload and verify
	svc2, err := NewConfigService(configPath)
	require.NoError(t, err)

	val, err := svc2.Get("project.name")
	require.NoError(t, err)
	assert.Equal(t, "my-project", val)
}
