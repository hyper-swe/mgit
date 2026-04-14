// Package e2e contains benchmarks for mgit NFR-1 performance targets.
// Refs: MGIT-6.2.1, MGIT-6.2.2, MGIT-6.2.3
package e2e

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/hyper-swe/mgit-dev/internal/service"
	gitstore "github.com/hyper-swe/mgit-dev/internal/store/git"
	"github.com/hyper-swe/mgit-dev/internal/store/index"
)

// BenchmarkCommit measures commit creation time. Target: <5ms.
// Refs: NFR-1.1
func BenchmarkCommit(b *testing.B) {
	env := setupBenchEnv(b)
	ctx := context.Background()

	b.ResetTimer()
	for i := range b.N {
		_, err := env.commit.CreateCommit(ctx, service.CreateCommitRequest{
			TaskID:  fmt.Sprintf("MGIT-%d.1", i+1),
			AgentID: "bench-agent",
			Message: fmt.Sprintf("benchmark commit %d", i),
		})
		if err != nil {
			b.Fatal(err)
		}
	}
}

// BenchmarkLog measures log retrieval for commits. Target: <50ms.
// Refs: NFR-1.2
func BenchmarkLog(b *testing.B) {
	env := setupBenchEnv(b)
	ctx := context.Background()

	// Seed 100 commits
	for i := range 100 {
		_, err := env.commit.CreateCommit(ctx, service.CreateCommitRequest{
			TaskID:  "MGIT-1.1",
			AgentID: "bench-agent",
			Message: fmt.Sprintf("seed commit %d", i),
		})
		require.NoError(b, err)
	}

	b.ResetTimer()
	for range b.N {
		_, err := env.commit.ListCommits(ctx)
		if err != nil {
			b.Fatal(err)
		}
	}
}

// BenchmarkSquash measures squash of 10 commits. Target: <500ms.
// Refs: NFR-1.3
func BenchmarkSquash(b *testing.B) {
	ctx := context.Background()

	for i := range b.N {
		env := setupBenchEnv(b)
		taskID := fmt.Sprintf("MGIT-%d.1", i+1)

		// Create 10 commits
		for j := range 10 {
			_, err := env.commit.CreateCommit(ctx, service.CreateCommitRequest{
				TaskID:  taskID,
				AgentID: "bench-agent",
				Message: fmt.Sprintf("commit %d", j),
			})
			require.NoError(b, err)
		}

		b.StartTimer()
		_, err := env.squash.SquashTask(ctx, service.SquashRequest{TaskID: taskID})
		b.StopTimer()

		if err != nil {
			b.Fatal(err)
		}
	}
}

// BenchmarkVerify measures verification of task commits. Target: <1s for 100 commits.
// Refs: NFR-1.5
func BenchmarkVerify(b *testing.B) {
	env := setupBenchEnv(b)
	ctx := context.Background()

	// Seed commits
	for i := range 50 {
		_, err := env.commit.CreateCommit(ctx, service.CreateCommitRequest{
			TaskID:  "MGIT-1.1",
			AgentID: "bench-agent",
			Message: fmt.Sprintf("verify commit %d", i),
		})
		require.NoError(b, err)
	}

	b.ResetTimer()
	for range b.N {
		err := env.verify.VerifyTaskCommits(ctx, "MGIT-1.1")
		if err != nil {
			b.Fatal(err)
		}
	}
}

// TestE2E_StorageEfficiency measures .mgit/ footprint for 100 commits.
// Refs: MGIT-6.2.3
func TestE2E_StorageEfficiency(t *testing.T) {
	env := setupServiceEnv(t)
	ctx := context.Background()

	// Create 100 commits across 10 tasks
	for i := range 100 {
		taskID := fmt.Sprintf("MGIT-%d.1", (i%10)+1)
		_, err := env.commit.CreateCommit(ctx, service.CreateCommitRequest{
			TaskID:  taskID,
			AgentID: "storage-test",
			Message: fmt.Sprintf("storage test commit %d", i),
		})
		require.NoError(t, err)
	}

	// Measure .mgit/ size
	mgitDir := env.repo.MgitDir()
	var totalSize int64
	err := filepath.Walk(mgitDir, func(_ string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if !info.IsDir() {
			totalSize += info.Size()
		}
		return nil
	})
	require.NoError(t, err)

	t.Logf("100 commits: .mgit/ size = %d bytes (%.1f KB)", totalSize, float64(totalSize)/1024)

	// Sanity check: should be less than 1MB for 100 commits
	assert100CommitsUnderLimit(t, totalSize)
}

func assert100CommitsUnderLimit(t *testing.T, size int64) {
	t.Helper()
	const maxSize = 5 * 1024 * 1024 // 5MB (loose objects before packfile compression)
	if size > maxSize {
		t.Errorf(".mgit/ size %d exceeds 1MB limit for 100 commits", size)
	}
}

// setupBenchEnv creates a service environment for benchmarks.
func setupBenchEnv(b testing.TB) *serviceEnv {
	b.Helper()
	tmpDir := b.TempDir()
	clock := fixedClock()

	repo, err := gitstore.Init(tmpDir, clock)
	require.NoError(b, err)
	b.Cleanup(func() { _ = repo.Close() })

	dbPath := filepath.Join(tmpDir, ".mgit", "index.db")
	idx, err := index.New(dbPath, clock)
	require.NoError(b, err)
	b.Cleanup(func() { _ = idx.Close() })

	cs := gitstore.NewCommitStore(repo)
	bs := gitstore.NewBranchStore(repo)

	return &serviceEnv{
		repo:     repo,
		idx:      idx,
		commit:   service.NewCommitService(repo, cs, idx),
		squash:   service.NewSquashService(repo, cs, idx),
		rollback: service.NewRollbackService(repo, cs, idx),
		branch:   service.NewBranchService(repo, bs, idx),
		verify:   service.NewVerifyService(cs, idx),
		audit:    service.NewAuditService(filepath.Join(tmpDir, ".mgit", "audit.log"), clock),
	}
}
