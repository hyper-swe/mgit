//go:build linux

// Full-stack guest->host auto-land trigger round-trip on real KVM: boot a guest
// whose mounted worktree carries a private .mgit store with a new commit, wire
// the host NotifyController (per-VM listener -> authorize -> verified land), and
// prove the GUEST's land-ready notification causes the host to auto-land — going
// through the SAME verified LandOrchestrator path as `mgit sandbox land`, with
// the guest holding no key (SEC-01) and the inbound peer authorized per-VM
// (SEC-10, F-E). Gated, like the land round-trip, on a prebuilt mgit-guest
// rootfs (which must emit the notify) via MGIT_E2E_GUEST_ROOTFS + /dev/kvm.
// Refs: MGIT-11.10.11, SEC-01, SEC-10, SEC-03
package firecracker

import (
	"context"
	"log/slog"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/hyper-swe/mgit/internal/model"
	"github.com/hyper-swe/mgit/internal/sandboxd"
	"github.com/hyper-swe/mgit/internal/sandboxd/images"
	"github.com/hyper-swe/mgit/internal/sandboxd/land"
	"github.com/hyper-swe/mgit/internal/sandboxd/provision"
	"github.com/hyper-swe/mgit/internal/service"
	gitstore "github.com/hyper-swe/mgit/internal/store/git"
	"github.com/hyper-swe/mgit/internal/store/index"
)

// landerAdapter adapts a *service.LandService to sandboxd.NotifyLander so the
// notify controller forwards an authorized trigger to the verified land path.
type landerAdapter struct{ svc *service.LandService }

func (a landerAdapter) Land(ctx context.Context, taskID string) (int, string, error) {
	s, err := a.svc.Land(ctx, taskID)
	if err != nil {
		return 0, "", err
	}
	return s.Commits, s.Branch, nil
}

// TestE2E_Notify_RealGuest_AutoLand proves the guest->host trigger end to end:
// the guest commits, dials the host notify port, and the host auto-lands the
// new commit onto its task branch through the verified orchestrator — with no
// host-initiated `mgit sandbox land` call. Refs: MGIT-11.10.11, SEC-01, SEC-10
func TestE2E_Notify_RealGuest_AutoLand(t *testing.T) {
	kernel, _ := requireKVM(t)
	rootfs := os.Getenv("MGIT_E2E_GUEST_ROOTFS")
	if rootfs == "" {
		t.Skip("set MGIT_E2E_GUEST_ROOTFS to a guest image (with the notify emitter) to run the auto-land round-trip")
	}
	if !fileExists(rootfs) {
		t.Skipf("guest rootfs %s absent", rootfs)
	}
	const task = "MGIT-11.10.11"
	clock := func() time.Time { return time.Now().UTC() }
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))

	// Host shared repo + task branch at the shared base (the auto-land fast-forwards it).
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

	// Materialize the worktree as plain files + a private .mgit store carrying
	// the agent's new commit (the SEC-03 model the guest serves).
	wtPath := filepath.Join(t.TempDir(), "repo", "worktrees", "task-a")
	require.NoError(t, gitstore.NewWorktreeStore(hostRepo).MaterializeBranchTo(context.Background(), model.TaskBranchName(task), wtPath))
	prov, err := provision.NewStoreProvisioner(hostRepoRoot)
	require.NoError(t, err)
	privDir := filepath.Join(wtPath, ".mgit")
	_, err = prov.Provision(task, privDir)
	require.NoError(t, err)
	newCommit := commitIntoPrivateStore(t, privDir, task)

	mainIdx, err := index.New(filepath.Join(hostRepoRoot, ".mgit", "index.db"), clock)
	require.NoError(t, err)
	t.Cleanup(func() { _ = mainIdx.Close() })

	// Register + boot the guest image.
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

	workDir, err := os.MkdirTemp("", "mgntfy") // short path for the vsock socket
	require.NoError(t, err)
	t.Cleanup(func() { _ = os.RemoveAll(workDir) })

	// Wire the verified land path and the host notify controller. The controller
	// is the manager's NotifyRegistrar: it opens a per-VM reverse-vsock listener
	// at launch and authorizes the inbound peer (SEC-10) before triggering land.
	binder := sandboxd.NewPeerBinder(logger)
	channel := sandboxd.NewLandChannel(binder, NewLandDialer(workDir), land.DefaultLimits(), logger)
	parents := land.NewPoolAwareParentResolver(land.NewHostParentTreeResolver(hostRepo))
	lander := land.NewLander(land.NewStoreImporter(hostRepo), mainIdx, land.NewStoreBrancher(ms))
	orch, err := service.NewLandOrchestrator(channel, e2eStubAttestor{}, lander, parents,
		mainIdx, e2eOffPolicy{}, land.DefaultLimits(), clock)
	require.NoError(t, err)

	notifyCtrl, err := sandboxd.NewNotifyController(binder, sandboxd.UnixListen, logger)
	require.NoError(t, err)

	mgr, err := NewManager(Config{
		WorkDir: workDir,
		Resolve: func(r string) (ImagePaths, error) {
			ri, rerr := store.Resolve(r)
			return ImagePaths{KernelPath: ri.KernelPath, RootfsPath: ri.RootfsPath, Cmdline: ri.Cmdline}, rerr
		},
		Logger: logger, Clock: clock, PeerBinder: binder, NotifyRegistrar: notifyCtrl,
	})
	require.NoError(t, err)

	info, err := mgr.Launch(context.Background(), model.SandboxLaunchOptions{
		TaskID: task, WorktreePath: wtPath, ImageRef: ref,
		Network: model.NetworkPolicy{Mode: model.NetworkModeNone}, CPUs: 1, MemoryMB: 256,
	})
	require.NoError(t, err)
	t.Cleanup(func() { _ = mgr.Remove(context.Background(), info.ID, true) })

	// The land service resolves the bound sandbox by id; the notify controller
	// resolves the host-bound task itself, then forwards to this verified land.
	landSvc, err := service.NewLandService(e2eStubResolver{id: info.ID}, channel, mainIdx,
		parents, e2eStubAttestor{}, orch, e2eOffPolicy{})
	require.NoError(t, err)
	notifyCtrl.SetLander(landerAdapter{svc: landSvc})

	// mgit-guest emits the land-ready notify after each completed exec (mirrors
	// an agent finishing a command), so drive ONE exec to fire it; the guest
	// boots+serves vsock asynchronously, so retry until it lands. No host-side
	// land call is made — the auto-land is triggered solely by the guest's
	// post-exec notify.
	execDeadline := time.Now().Add(25 * time.Second)
	for time.Now().Before(execDeadline) {
		if _, eerr := mgr.Exec(context.Background(), info.ID,
			model.ExecRequest{Command: []string{"/bin/sh", "-c", "true"}}); eerr == nil {
			break
		}
		time.Sleep(400 * time.Millisecond)
	}

	// The host per-VM listener authorizes the guest's notify (SEC-10) and
	// auto-lands. Poll the task branch until it advances to the agent's commit.
	deadline := time.Now().Add(40 * time.Second)
	var tip *model.Branch
	for time.Now().Before(deadline) {
		tip, err = branches.GetBranch(context.Background(), model.TaskBranchName(task))
		if err == nil && tip.HeadCommit == newCommit {
			break
		}
		time.Sleep(500 * time.Millisecond)
	}
	require.NoError(t, err)
	assert.Equal(t, newCommit, tip.HeadCommit, "the guest's notify auto-landed the commit (branch advanced)")

	recs, err := mainIdx.GetTaskCommits(context.Background(), task)
	require.NoError(t, err)
	require.Len(t, recs, 1, "the auto-land recorded the commit in the ledger")
	assert.Equal(t, newCommit, recs[0].CommitHash)
}
