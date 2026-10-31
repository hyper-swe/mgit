package main

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"testing"

	gogit "github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/plumbing"
	"github.com/hyper-swe/mgit/internal/model"
	gitstore "github.com/hyper-swe/mgit/internal/store/git"
	"github.com/hyper-swe/mgit/internal/store/index"
	"github.com/stretchr/testify/require"
)

func appendProjectionPending(t *testing.T, f projectionFixture) {
	t.Helper()
	require.NoError(t, f.idx.AppendTaskCommits(context.Background(), []index.TaskCommitInsert{{TaskID: "MGIT-317", CommitHash: f.tip, ContentHash: "host-attested", Origin: index.OriginLand, GuestContentHash: "guest-claimed"}}))
}

func TestLandAdmission_RefusalThenZeroNewRetry(t *testing.T) {
	f := newProjectionFixture(t)
	ctx := context.Background()
	appendProjectionPending(t, f)
	require.NoError(t, os.WriteFile(filepath.Join(f.native, "existing.txt"), []byte("host WIP\n"), 0600))
	a := landAdapter{svc: projectionSummaryStub{}, projection: f.p}
	_, _, err := a.Land(ctx, "MGIT-317")
	require.Error(t, err)
	require.ErrorContains(t, f.idx.RequireContentCommit(ctx, f.tip), "undelivered")
	raw, err := f.idx.GetTaskCommits(ctx, "MGIT-317")
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(filepath.Join(f.native, "existing.txt"), []byte("base\n"), 0600))
	n, _, err := a.Land(ctx, "MGIT-317")
	require.NoError(t, err)
	require.Zero(t, n)
	require.NoError(t, f.idx.RequireContentCommit(ctx, f.tip))
	after, err := f.idx.GetTaskCommits(ctx, "MGIT-317")
	require.NoError(t, err)
	require.Equal(t, raw, after)
	_, _, err = a.Land(ctx, "MGIT-317")
	require.NoError(t, err)
}

func TestLandAdmission_ProjectionRecordCrashRecovery(t *testing.T) {
	f := newProjectionFixture(t)
	ctx := context.Background()
	appendProjectionPending(t, f)
	// Simulate a crash AFTER the actual native ref/disk/index projection and
	// BEFORE its final delivery event. Only the trusted projection may recover.
	native, err := gitstore.OpenLinked(f.native, f.repo.MgitDir(), model.TaskBranchName("MGIT-317"), f.p.clock)
	require.NoError(t, err)
	require.NoError(t, gitstore.NewMergeStore(native).FastForwardDelivered(ctx, model.TaskBranchName("MGIT-317"), f.tip))
	require.NoError(t, native.Close())
	require.ErrorContains(t, f.idx.RequireContentCommit(ctx, f.tip), "undelivered")
	require.NoError(t, f.p.Check(ctx, "MGIT-317"))
	require.NoError(t, f.idx.RequireContentCommit(ctx, f.tip))
}

func TestLandAdmission_BranchResetThenZeroNewRetry(t *testing.T) {
	f := newProjectionFixture(t)
	ctx := context.Background()
	native, err := gitstore.OpenLinked(f.native, f.repo.MgitDir(), model.TaskBranchName("MGIT-317"), f.p.clock)
	require.NoError(t, err)
	t.Cleanup(func() { _ = native.Close() })
	require.NoError(t, os.WriteFile(filepath.Join(f.native, "existing.txt"), []byte("host advanced\n"), 0600))
	require.NoError(t, gitstore.NewWorktreeStore(native).Add(ctx, "existing.txt"))
	host, err := gitstore.NewCommitStore(native).CreateCommit(ctx, &model.Commit{AgentID: "host", Message: "host advancement"})
	require.NoError(t, err)
	appendProjectionPending(t, f)
	a := landAdapter{svc: projectionSummaryStub{}, projection: f.p}
	_, _, err = a.Land(ctx, "MGIT-317")
	require.ErrorContains(t, err, "non-fast-forward")
	require.ErrorContains(t, f.idx.RequireContentCommit(ctx, f.tip), "undelivered")
	branch, err := gitstore.NewBranchStore(f.repo).GetBranch(ctx, model.TaskBranchName("MGIT-317"))
	require.NoError(t, err)
	require.Equal(t, host, branch.HeadCommit)
	// Model an explicit operator reset in this disposable fixture. No production
	// admission decision may use that reset alone as evidence of delivery.
	bare, err := gogit.PlainOpen(f.repo.MgitDir())
	require.NoError(t, err)
	require.NoError(t, bare.Storer.SetReference(plumbing.NewHashReference(plumbing.NewBranchReferenceName(model.TaskBranchName("MGIT-317")), plumbing.NewHash(f.base))))
	require.NoError(t, os.WriteFile(filepath.Join(f.native, "existing.txt"), []byte("base\n"), 0600))
	require.ErrorContains(t, f.idx.RequireContentCommit(ctx, f.tip), "undelivered")
	n, _, err := a.Land(ctx, "MGIT-317")
	require.NoError(t, err)
	require.Zero(t, n)
	require.NoError(t, f.idx.RequireContentCommit(ctx, f.tip))
	raw, err := f.idx.GetTaskCommits(ctx, "MGIT-317")
	require.NoError(t, err)
	require.Len(t, raw, 1)
	_, _, err = a.Land(ctx, "MGIT-317")
	require.NoError(t, err)
	raw, err = f.idx.GetTaskCommits(ctx, "MGIT-317")
	require.NoError(t, err)
	require.Len(t, raw, 1)
}

func TestLandAdmission_LegacyRetryCannotProject(t *testing.T) {
	f := newProjectionFixture(t)
	ctx := context.Background()
	require.NoError(t, f.idx.AppendTaskCommit(ctx, index.TaskCommitInsert{TaskID: "MGIT-317", CommitHash: f.tip, ContentHash: "legacy-unknown"}))
	a := landAdapter{svc: projectionSummaryStub{}, projection: f.p}
	_, _, err := a.Land(ctx, "MGIT-317")
	require.ErrorContains(t, err, "ambiguous content eligibility")
	require.ErrorContains(t, err, "mgit init")
	require.ErrorContains(t, err, "mgit commit -a --task-id RECOVERY-1")
	branch, err := gitstore.NewBranchStore(f.repo).GetBranch(ctx, model.TaskBranchName("MGIT-317"))
	require.NoError(t, err)
	require.Equal(t, f.base, branch.HeadCommit)
	data, err := os.ReadFile(filepath.Join(f.native, "existing.txt"))
	require.NoError(t, err)
	require.Equal(t, "base\n", string(data))
	_, err = os.Stat(filepath.Join(f.native, "new.txt"))
	require.True(t, os.IsNotExist(err))
}

// The exact inspected-file recovery also succeeds after legacy land refuses.
// This uses the real CLI, not a fake new-task admission or an origin backfill.
func TestLandAdmission_LegacyRecoveryCommandSucceeds(t *testing.T) {
	f := newProjectionFixture(t)
	ctx := context.Background()
	require.NoError(t, f.idx.AppendTaskCommit(ctx, index.TaskCommitInsert{TaskID: "MGIT-317", CommitHash: f.tip, ContentHash: "legacy"}))
	adapter := landAdapter{svc: projectionSummaryStub{}, projection: f.p}
	_, _, err := adapter.Land(ctx, "MGIT-317")
	require.ErrorContains(t, err, index.ContentRecoveryHint)
	_, source, _, ok := runtime.Caller(0)
	require.True(t, ok)
	cli := filepath.Join(t.TempDir(), "mgit")
	build := exec.CommandContext(t.Context(), "go", "build", "-buildvcs=false", "-o", cli, "./cmd/mgit") //nolint:gosec // G204: fixed build target or test-built CLI with fixture-owned arguments
	build.Dir = filepath.Clean(filepath.Join(filepath.Dir(source), "..", ".."))
	out, err := build.CombinedOutput()
	require.NoError(t, err, string(out))
	recovery := t.TempDir()
	run := func(args ...string) string {
		t.Helper()
		cmd := exec.CommandContext(t.Context(), cli, args...) //nolint:gosec // G204: fixed build target or test-built CLI with fixture-owned arguments
		cmd.Dir = recovery
		out, err := cmd.CombinedOutput()
		require.NoError(t, err, string(out))
		return string(out)
	}
	run("init")
	inspected, err := os.ReadFile(filepath.Join(f.native, "existing.txt"))
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(filepath.Join(recovery, "existing.txt"), inspected, 0600)) //nolint:gosec // G703: fixed filename under a test-owned empty directory
	run("commit", "-a", "--task-id", "RECOVERY-1", "-m", "Recover inspected files")
	require.Contains(t, run("export", "RECOVERY-1", "--format", "git"), "+base")
	data, err := os.ReadFile(filepath.Join(f.native, "existing.txt"))
	require.NoError(t, err)
	require.Equal(t, inspected, data)
	require.ErrorContains(t, f.idx.RequireContentCommit(ctx, f.tip), "ambiguous content eligibility")
}
