//go:build linux

// Full-stack land round-trip against a real mgit-guest image: boot a guest
// whose mounted worktree is a git repo with a new commit, then pull that
// commit over the guest land channel, verify it host-side, and atomically
// import + fast-forward the host task branch — exercising guest land server
// -> host pull -> verify -> import -> fast-forward end to end. Gated, like
// the exec round-trip, on a prebuilt mgit-guest rootfs (which must include
// the land listener) via MGIT_E2E_GUEST_ROOTFS. Refs: FR-17.5, MGIT-11.10.10
package firecracker

import (
	"context"
	"log/slog"
	"os"
	"path/filepath"
	"testing"
	"time"

	gogit "github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/plumbing/object"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/hyper-swe/mgit/internal/model"
	"github.com/hyper-swe/mgit/internal/sandboxd"
	"github.com/hyper-swe/mgit/internal/sandboxd/images"
	"github.com/hyper-swe/mgit/internal/sandboxd/land"
	"github.com/hyper-swe/mgit/internal/service"
	gitstore "github.com/hyper-swe/mgit/internal/store/git"
	"github.com/hyper-swe/mgit/internal/store/index"
)

// e2eStubResolver resolves the bound sandbox by its launched id.
type e2eStubResolver struct{ id string }

func (r e2eStubResolver) Status(context.Context, string) (*model.SandboxInfo, error) {
	return &model.SandboxInfo{ID: r.id}, nil
}

// e2eOffPolicy disables require_sandbox so the round-trip needs no attestation
// (the land import path itself is what this proves).
type e2eOffPolicy struct{}

func (e2eOffPolicy) Load(context.Context) (model.SandboxPolicy, error) {
	return model.SandboxPolicy{RequireSandbox: false}, nil
}

// e2eStubAttestor is never consulted under require_sandbox=off; it satisfies
// both the orchestrator's verifier and the land service's attestor.
type e2eStubAttestor struct{}

func (e2eStubAttestor) Attest(_ context.Context, sandboxID, commitHash, contentHash string) (*model.Attestation, error) {
	return &model.Attestation{SandboxID: sandboxID, CommitHash: commitHash, ContentHash: contentHash}, nil
}
func (e2eStubAttestor) Verify(context.Context, *model.Attestation) error { return nil }

// TestE2E_Land_RealGuest_RoundTrip proves the whole land stack on real KVM:
// a guest serves its worktree's new commit over the land channel, and the
// host pulls, verifies (dual-hash + tree binding), imports, and fast-forwards
// the task branch. The guest holds no key; the host makes every decision.
func TestE2E_Land_RealGuest_RoundTrip(t *testing.T) {
	kernel, _ := requireKVM(t)
	rootfs := os.Getenv("MGIT_E2E_GUEST_ROOTFS")
	if rootfs == "" {
		t.Skip("set MGIT_E2E_GUEST_ROOTFS to a guest image (with the land listener) to run the land round-trip")
	}
	if !fileExists(rootfs) {
		t.Skipf("guest rootfs %s absent", rootfs)
	}
	const task = "MGIT-11.10.10"
	clock := func() time.Time { return time.Now().UTC() }
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))

	// Host shared repo + main index. The worktree is a clone of it plus one
	// new commit (the agent's work); the task branch is pre-created at the
	// shared base so the land fast-forwards it. Refs: FR-17.5
	hostRepoRoot := t.TempDir()
	hostRepo, err := gitstore.Init(hostRepoRoot, clock)
	require.NoError(t, err)
	t.Cleanup(func() { _ = hostRepo.Close() })
	base, err := hostRepo.Head()
	require.NoError(t, err)
	ms := gitstore.NewMergeStore(hostRepo)
	branches := gitstore.NewBranchStore(hostRepo)
	require.NoError(t, branches.CreateBranch(context.Background(),
		&model.Branch{Name: model.TaskBranchName(task), HeadCommit: base}))

	wtPath := filepath.Join(t.TempDir(), "repo", "worktrees", "task-a")
	newCommit := cloneAndCommit(t, hostRepoRoot, wtPath)

	mainIdx, err := index.New(filepath.Join(hostRepoRoot, ".mgit", "index.db"), clock)
	require.NoError(t, err)
	t.Cleanup(func() { _ = mainIdx.Close() })

	// Register the guest image and boot it with the worktree mounted.
	hostRoot := t.TempDir()
	_, err = images.GenerateTrustRoot(context.Background(), hostRoot, noopAudit{})
	require.NoError(t, err)
	priv, err := images.LoadSigningKey(hostRoot)
	require.NoError(t, err)
	entry, err := images.BuildEntry(kernel, rootfs, e2eGuestCmdline)
	require.NoError(t, err)
	ref, err := images.Register(hostRoot, "mgit-guest", entry, priv)
	require.NoError(t, err)
	store, err := images.NewStore(hostRoot, clock)
	require.NoError(t, err)

	workDir, err := os.MkdirTemp("", "mgland") // short path for the vsock socket
	require.NoError(t, err)
	t.Cleanup(func() { _ = os.RemoveAll(workDir) })

	binder := sandboxd.NewPeerBinder(logger)
	mgr, err := NewManager(Config{
		WorkDir: workDir,
		Resolve: func(r string) (ImagePaths, error) {
			ri, rerr := store.Resolve(r)
			return ImagePaths{KernelPath: ri.KernelPath, RootfsPath: ri.RootfsPath, Cmdline: ri.Cmdline}, rerr
		},
		Logger: logger, Clock: clock, PeerBinder: binder,
	})
	require.NoError(t, err)

	info, err := mgr.Launch(context.Background(), model.SandboxLaunchOptions{
		TaskID: task, WorktreePath: wtPath, ImageRef: ref,
		Network: model.NetworkPolicy{Mode: model.NetworkModeNone}, CPUs: 1, MemoryMB: 256,
	})
	require.NoError(t, err)
	t.Cleanup(func() { _ = mgr.Remove(context.Background(), info.ID, true) })

	// Wire the verified land path over the booted guest.
	channel := sandboxd.NewLandChannel(binder, NewLandDialer(workDir), land.DefaultLimits(), logger)
	parents := land.NewPoolAwareParentResolver(land.NewHostParentTreeResolver(hostRepo))
	lander := land.NewLander(land.NewStoreImporter(hostRepo), mainIdx, land.NewStoreBrancher(ms))
	orch, err := service.NewLandOrchestrator(channel, e2eStubAttestor{}, lander, parents,
		mainIdx, e2eOffPolicy{}, land.DefaultLimits(), clock)
	require.NoError(t, err)
	landSvc, err := service.NewLandService(e2eStubResolver{id: info.ID}, channel, mainIdx,
		parents, e2eStubAttestor{}, orch, e2eOffPolicy{})
	require.NoError(t, err)

	// The guest serves the land port asynchronously after boot; retry until it
	// is listening, then assert the new commit landed + the branch advanced.
	var sum *service.LandSummary
	deadline := time.Now().Add(25 * time.Second)
	for time.Now().Before(deadline) {
		sum, err = landSvc.Land(context.Background(), task)
		if err == nil && sum.Commits > 0 {
			break
		}
		time.Sleep(400 * time.Millisecond)
	}
	require.NoError(t, err, "land must reach the guest once it is serving the land channel")
	require.Equal(t, 1, sum.Commits, "the agent's new commit lands")

	// The host task branch fast-forwarded to the landed commit.
	tip, err := branches.GetBranch(context.Background(), model.TaskBranchName(task))
	require.NoError(t, err)
	assert.Equal(t, newCommit, tip.HeadCommit, "the task branch advanced to the landed commit")

	// task_commits recorded the landed commit (durable provenance).
	recs, err := mainIdx.GetTaskCommits(context.Background(), task)
	require.NoError(t, err)
	require.Len(t, recs, 1)
	assert.Equal(t, newCommit, recs[0].CommitHash)
}

// cloneAndCommit clones the host repo into wtPath and adds one new commit
// (the agent's work) on top of the shared base, returning its hash. The clone
// source is the self-contained .mgit store: post-MGIT-14 mgit no longer writes
// a .git at the project root (it coexists with the project's git via a bare
// .mgit store), so the worktree is cloned from that store. Refs: MGIT-14
func cloneAndCommit(t *testing.T, hostRepoRoot, wtPath string) string {
	t.Helper()
	require.NoError(t, os.MkdirAll(filepath.Dir(wtPath), 0o750))
	repo, err := gogit.PlainClone(wtPath, false, &gogit.CloneOptions{URL: filepath.Join(hostRepoRoot, ".mgit")})
	require.NoError(t, err)
	wt, err := repo.Worktree()
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(filepath.Join(wtPath, "work.txt"), []byte("agent work"), 0o600))
	_, err = wt.Add("work.txt")
	require.NoError(t, err)
	h, err := wt.Commit("feat: agent work", &gogit.CommitOptions{
		Author: &object.Signature{Name: "agent", Email: "agent@mgit", When: time.Now().UTC()},
	})
	require.NoError(t, err)
	return h.String()
}
