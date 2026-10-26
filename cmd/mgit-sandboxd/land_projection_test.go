package main

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/hyper-swe/mgit/internal/model"
	"github.com/hyper-swe/mgit/internal/service"
	gitstore "github.com/hyper-swe/mgit/internal/store/git"
	"github.com/hyper-swe/mgit/internal/store/index"
	"github.com/stretchr/testify/require"
)

type projectionResolver struct{ path string }

func (r projectionResolver) Status(context.Context, string) (*model.SandboxInfo, error) {
	return &model.SandboxInfo{ID: "sandbox", TaskID: "MGIT-317", WorktreePath: r.path}, nil
}

type projectionFixture struct {
	p                       *landProjection
	root, native, tip, base string
	idx                     *index.Store
	repo                    *gitstore.Repository
}

func newProjectionFixture(t *testing.T) projectionFixture {
	t.Helper()
	ctx := context.Background()
	clock := func() time.Time { return time.Unix(1, 0) }
	root, native, guest := t.TempDir(), t.TempDir(), t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(root, "existing.txt"), []byte("base\n"), 0600))
	repo, err := gitstore.Init(root, clock)
	require.NoError(t, err)
	t.Cleanup(func() { _ = repo.Close() })
	base, err := repo.Head()
	require.NoError(t, err)
	bs := gitstore.NewBranchStore(repo)
	require.NoError(t, bs.CreateBranch(ctx, &model.Branch{Name: model.TaskBranchName("MGIT-317"), HeadCommit: base}))
	require.NoError(t, bs.CreateBranch(ctx, &model.Branch{Name: "guest", HeadCommit: base}))
	ws := gitstore.NewWorktreeStore(repo)
	require.NoError(t, ws.WriteWorktreeMarker(guest, "guest", "MGIT-317"))
	require.NoError(t, ws.WriteWorktreeMarker(native, model.TaskBranchName("MGIT-317"), "MGIT-317"))
	require.NoError(t, os.WriteFile(filepath.Join(native, "existing.txt"), []byte("base\n"), 0600))
	gr, err := gitstore.OpenLinked(guest, repo.MgitDir(), "guest", clock)
	require.NoError(t, err)
	t.Cleanup(func() { _ = gr.Close() })
	require.NoError(t, os.WriteFile(filepath.Join(guest, "existing.txt"), []byte("guest\n"), 0600))
	require.NoError(t, os.WriteFile(filepath.Join(guest, "new.txt"), []byte("new\n"), 0600))
	gws := gitstore.NewWorktreeStore(gr)
	require.NoError(t, gws.Add(ctx, "existing.txt"))
	require.NoError(t, gws.Add(ctx, "new.txt"))
	tip, err := gitstore.NewCommitStore(gr).CreateCommit(ctx, &model.Commit{AgentID: "test", Message: "test: guest"})
	require.NoError(t, err)
	idx, err := index.New(filepath.Join(repo.MgitDir(), "index.db"), clock)
	require.NoError(t, err)
	t.Cleanup(func() { _ = idx.Close() })
	require.NoError(t, idx.InsertWorktree(ctx, &model.WorktreeInfo{Path: native, Branch: model.TaskBranchName("MGIT-317"), TaskID: "MGIT-317"}))
	p := &landProjection{repoRoot: root, resolver: projectionResolver{native}, idx: idx, clock: clock}
	return projectionFixture{p, root, native, tip, base, idx, repo}
}

func TestLandProjection_NativeDeliveryAndRetry(t *testing.T) {
	f := newProjectionFixture(t)
	ctx := context.Background()
	require.NoError(t, os.WriteFile(filepath.Join(f.native, "unrelated.txt"), []byte("host only\n"), 0600))
	require.NoError(t, f.p.FastForward(ctx, "MGIT-317", f.tip))
	for path, want := range map[string]string{"existing.txt": "guest\n", "new.txt": "new\n"} {
		b, err := os.ReadFile(filepath.Join(f.native, path)) //nolint:gosec // G304: fixed fixture paths
		require.NoError(t, err)
		require.Equal(t, want, string(b))
	}
	b, err := os.ReadFile(filepath.Join(f.root, "existing.txt"))
	require.NoError(t, err)
	require.Equal(t, "base\n", string(b))
	_, err = os.Stat(filepath.Join(f.root, "new.txt"))
	require.True(t, os.IsNotExist(err))
	require.NoError(t, f.p.FastForward(ctx, "MGIT-317", f.tip), "same tip is idempotent")
	require.ErrorContains(t, f.p.FastForward(ctx, "MGIT-317", f.base), "non-fast-forward")
}

func TestLandProjection_RefusesHostEditsAndCollisions(t *testing.T) {
	for _, kind := range []string{"modified", "deleted", "untracked collision", "marker task", "marker store", "marker branch", "missing marker", "registry mismatch"} {
		t.Run(kind, func(t *testing.T) {
			f := newProjectionFixture(t)
			switch kind {
			case "modified":
				require.NoError(t, os.WriteFile(filepath.Join(f.native, "existing.txt"), []byte("host edit\n"), 0600))
			case "deleted":
				require.NoError(t, os.Remove(filepath.Join(f.native, "existing.txt")))
			case "untracked collision":
				require.NoError(t, os.WriteFile(filepath.Join(f.native, "new.txt"), []byte("host untracked\n"), 0600))
			case "marker task", "marker store", "marker branch":
				m, _, err := gitstore.ReadWorktreeMarker(f.native)
				require.NoError(t, err)
				switch kind {
				case "marker task":
					m.Task = "MGIT-999"
				case "marker store":
					m.Store = t.TempDir()
				case "marker branch":
					m.Branch = "main"
				}
				// Write through the public marker API, preserving its schema.
				store := f.repo
				if kind == "marker store" {
					store, err = gitstore.Init(m.Store, time.Now)
					require.NoError(t, err)
					defer func() { _ = store.Close() }()
				}
				require.NoError(t, gitstore.NewWorktreeStore(store).WriteWorktreeMarker(f.native, m.Branch, m.Task))
			case "missing marker":
				require.NoError(t, os.Remove(filepath.Join(f.native, ".mgit", "worktree")))
			case "registry mismatch":
				f.p.resolver = projectionResolver{t.TempDir()}
			}
			require.Error(t, f.p.FastForward(context.Background(), "MGIT-317", f.tip))
			b, err := gitstore.NewBranchStore(f.repo).GetBranch(context.Background(), model.TaskBranchName("MGIT-317"))
			require.NoError(t, err)
			require.Equal(t, f.base, b.HeadCommit)
			if kind == "modified" {
				b, err := os.ReadFile(filepath.Join(f.native, "existing.txt"))
				require.NoError(t, err)
				require.Equal(t, "host edit\n", string(b))
			}
			if kind == "untracked collision" {
				b, err := os.ReadFile(filepath.Join(f.native, "new.txt"))
				require.NoError(t, err)
				require.Equal(t, "host untracked\n", string(b))
			}
		})
	}
}

type projectionSummaryStub struct{}

func (projectionSummaryStub) Land(context.Context, string) (*service.LandSummary, error) {
	return &service.LandSummary{Branch: model.TaskBranchName("MGIT-317")}, nil
}

func TestLandProjection_AlreadyImportedCannotSilentlySucceed(t *testing.T) {
	f := newProjectionFixture(t)
	ctx := context.Background()
	require.NoError(t, f.idx.AppendTaskCommits(ctx, []index.TaskCommitInsert{{TaskID: "MGIT-317", CommitHash: f.tip, ContentHash: "verified", AgentID: "test", Position: 0, SandboxID: "sandbox"}}))
	adapter := landAdapter{svc: projectionSummaryStub{}, projection: f.p}
	_, _, err := adapter.Land(ctx, "MGIT-317")
	require.NoError(t, err, "zero-new land must retry pending projection safely")
	n, _, err := adapter.Land(ctx, "MGIT-317")
	require.NoError(t, err)
	require.Zero(t, n)
	require.NoError(t, os.Remove(filepath.Join(f.native, "new.txt")))
	_, _, err = adapter.Land(ctx, "MGIT-317")
	require.ErrorContains(t, err, "native projection would overwrite")
	_, err = os.Stat(filepath.Join(f.native, "new.txt"))
	require.True(t, os.IsNotExist(err), "check must not restore over host changes")
}

func TestLandProjection_NormalRootAndMissingBinding(t *testing.T) {
	f := newProjectionFixture(t)
	ctx := context.Background()
	// A normal-root sandbox has no linked registration, but still publishes on
	// its task's logical HEAD, never the shared main HEAD.
	require.NoError(t, f.idx.DeleteWorktree(ctx, f.native))
	f.p.resolver = projectionResolver{f.root}
	require.NoError(t, f.p.FastForward(ctx, "MGIT-317", f.tip))
	b, err := os.ReadFile(filepath.Join(f.root, "new.txt"))
	require.NoError(t, err)
	require.Equal(t, "new\n", string(b))
	head, err := f.repo.Head()
	require.NoError(t, err)
	require.Equal(t, f.base, head)
	f.p.resolver = projectionResolver{""}
	require.ErrorContains(t, f.p.FastForward(ctx, "MGIT-317", f.tip), "binding is missing")
}

func TestLandProjection_SyncedHostAlreadyMatchesTip(t *testing.T) {
	f := newProjectionFixture(t)
	for path, data := range map[string]string{"existing.txt": "guest\n", "new.txt": "new\n"} {
		require.NoError(t, os.WriteFile(filepath.Join(f.native, path), []byte(data), 0600))
	}
	require.NoError(t, f.p.FastForward(context.Background(), "MGIT-317", f.tip))
}

func TestLandProjection_RetryAfterConflictCleanup(t *testing.T) {
	f := newProjectionFixture(t)
	ctx := context.Background()
	require.NoError(t, os.WriteFile(filepath.Join(f.native, "existing.txt"), []byte("unrelated host edit\n"), 0600))
	require.NoError(t, f.idx.AppendTaskCommits(ctx, []index.TaskCommitInsert{{TaskID: "MGIT-317", CommitHash: f.tip, ContentHash: "verified", AgentID: "test", Position: 0, SandboxID: "sandbox"}}))
	adapter := landAdapter{svc: projectionSummaryStub{}, projection: f.p}
	_, _, err := adapter.Land(ctx, "MGIT-317")
	require.Error(t, err)
	data, err := os.ReadFile(filepath.Join(f.native, "existing.txt"))
	require.NoError(t, err)
	require.Equal(t, "unrelated host edit\n", string(data))
	require.NoError(t, os.WriteFile(filepath.Join(f.native, "existing.txt"), []byte("base\n"), 0600))
	_, _, err = adapter.Land(ctx, "MGIT-317")
	require.NoError(t, err, "cleaning host state must make the public land retry finish projection")
	data, err = os.ReadFile(filepath.Join(f.native, "new.txt"))
	require.NoError(t, err)
	require.Equal(t, "new\n", string(data))
}

func TestProjectionShellQuote(t *testing.T) {
	require.Equal(t, "'./file'\"'\"'$(literal)'", projectionShellQuote("./file'$(literal)"))
}
