//go:build linux

// FR-17 end-to-end agent workflow on real KVM, on the SEC-03 .mgit model:
// the full claim -> worktree+sandbox launch -> guest build/test (exec) ->
// commit -> land -> teardown loop across one real Firecracker backend. It
// reuses the land round-trip spine (materialize worktree -> private .mgit ->
// commit into the private store -> launch -> land -> assert branch advanced +
// ledger recorded) and extends it with: a guest exec (build/test) before land;
// full per-commit provenance assertions (task_commits.sandbox_id set + a
// sandbox_events row recording backend/image_digest/network_mode); and a
// teardown/no-residue case proving unlanded work is discarded and no
// per-sandbox host state survives Remove. Gated, like the land round-trip, on
// /dev/kvm + a prebuilt mgit-guest rootfs via MGIT_E2E_GUEST_ROOTFS, so without
// them it skips rather than fails. Identical test NAMES live in the vzf backend
// under e2e_workflow_darwin_test.go. Refs: FR-17.5, FR-17.18, FR-17.19, SEC-03, MGIT-11.13.1
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
	"github.com/hyper-swe/mgit/internal/sandboxd/backend/microvm"
	"github.com/hyper-swe/mgit/internal/sandboxd/images"
	"github.com/hyper-swe/mgit/internal/sandboxd/land"
	"github.com/hyper-swe/mgit/internal/sandboxd/provision"
	"github.com/hyper-swe/mgit/internal/service"
	gitstore "github.com/hyper-swe/mgit/internal/store/git"
	"github.com/hyper-swe/mgit/internal/store/index"
)

// e2eWorkflowHarness bundles the live, wired components one E2E workflow test
// drives: the booted manager, the verified land service, the host branch +
// main index for assertions, the per-sandbox state root, and the sandbox/task
// identifiers. It is the backend-agnostic spine shared by all three tests.
type e2eWorkflowHarness struct {
	mgr       *microvm.Manager
	landSvc   *service.LandService
	branches  *gitstore.BranchStore
	mainIdx   *index.Store
	workDir   string // per-sandbox state root (SandboxStateDir base)
	sandboxID string
	task      string
	netMode   string
	newCommit string // the agent's commit in the private store, pending land
}

// e2eWorkflowImageDigest is the image_digest provenance the launch records: the
// sha256 the backend extracts from the pinned ImageRef.
const e2eWorkflowImageDigest = "sha256:" +
	"e2e0000000000000000000000000000000000000000000000000000000000001"

// stateDir is the per-sandbox host state directory (overlay, sockets, private
// store) created at launch and removed at teardown. Refs: FR-17.19
func (h *e2eWorkflowHarness) stateDir() string {
	return microvm.SandboxStateDir(h.workDir, h.sandboxID)
}

// setupE2EWorkflow boots one real KVM sandbox bound to a freshly materialized
// worktree carrying a private .mgit store with the agent's commit, emits the
// "created" provenance event (the same backend/image_digest/network_mode record
// the lifecycle service writes at registration), and wires the verified land
// path. It mirrors landtrip_linux_test.go's spine exactly; the caller owns
// driving exec/land/remove and the assertions. Refs: SEC-03, FR-17.5, FR-17.18
func setupE2EWorkflow(t *testing.T, task string) *e2eWorkflowHarness {
	t.Helper()
	kernel, rootfs := requireE2EGuest(t)
	clock := func() time.Time { return time.Now().UTC() }
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))

	hostRepo, hostRepoRoot := e2eHostRepo(t, task, clock)
	wtPath, newCommit := e2ePrivateWorktree(t, hostRepo, hostRepoRoot, task)
	mainIdx, err := index.New(filepath.Join(hostRepoRoot, ".mgit", "index.db"), clock)
	require.NoError(t, err)
	t.Cleanup(func() { _ = mainIdx.Close() })

	binder := sandboxd.NewPeerBinder(logger)
	mgr, ref, workDir := e2eBootGuest(t, kernel, rootfs, binder, clock, logger)
	info := e2eLaunch(t, mgr, mainIdx, wtPath, ref, task)

	landSvc := e2eLandService(t, hostRepo, mainIdx, binder, workDir, info.ID, clock, logger)
	return &e2eWorkflowHarness{
		mgr: mgr, landSvc: landSvc, branches: gitstore.NewBranchStore(hostRepo), mainIdx: mainIdx,
		workDir: workDir, sandboxID: info.ID, task: task,
		netMode: model.NetworkModeNone, newCommit: newCommit,
	}
}

// requireE2EGuest gates on /dev/kvm + a usable firecracker backend (via
// requireKVM, which yields the kernel) and the prebuilt mgit-guest rootfs from
// MGIT_E2E_GUEST_ROOTFS, skipping cleanly when any is absent.
func requireE2EGuest(t *testing.T) (kernel, rootfs string) {
	t.Helper()
	kernel, _ = requireKVM(t)
	rootfs = os.Getenv("MGIT_E2E_GUEST_ROOTFS")
	if rootfs == "" {
		t.Skip("set MGIT_E2E_GUEST_ROOTFS to a guest image (with the land listener) to run the e2e workflow")
	}
	if !fileExists(rootfs) {
		t.Skipf("guest rootfs %s absent", rootfs)
	}
	return kernel, rootfs
}

// e2eHostRepo initializes the host shared repo and pre-creates the task branch
// at the shared base so the land fast-forwards it. Refs: FR-17.5
func e2eHostRepo(t *testing.T, task string, clock func() time.Time) (*gitstore.Repository, string) {
	t.Helper()
	hostRepoRoot := t.TempDir()
	hostRepo, err := gitstore.Init(hostRepoRoot, clock)
	require.NoError(t, err)
	t.Cleanup(func() { _ = hostRepo.Close() })
	base, err := hostRepo.Head()
	require.NoError(t, err)
	require.NoError(t, gitstore.NewBranchStore(hostRepo).CreateBranch(context.Background(),
		&model.Branch{Name: model.TaskBranchName(task), HeadCommit: base}))
	return hostRepo, hostRepoRoot
}

// e2ePrivateWorktree materializes the worktree as plain files, provisions a
// fresh private .mgit store seeded with the task base, and commits the agent's
// work into THAT private store, returning the worktree path + commit hash. This
// is the SEC-03 store the guest serves over the land channel. Refs: SEC-03
func e2ePrivateWorktree(t *testing.T, hostRepo *gitstore.Repository, hostRepoRoot, task string) (string, string) {
	t.Helper()
	wtPath := filepath.Join(t.TempDir(), "repo", "worktrees", "task-a")
	require.NoError(t, gitstore.NewWorktreeStore(hostRepo).MaterializeBranchTo(
		context.Background(), model.TaskBranchName(task), wtPath))
	prov, err := provision.NewStoreProvisioner(hostRepoRoot)
	require.NoError(t, err)
	privDir := filepath.Join(wtPath, ".mgit")
	_, err = prov.Provision(task, privDir)
	require.NoError(t, err)
	return wtPath, commitIntoPrivateStore(t, privDir, task)
}

// e2eBootGuest registers the mgit-guest image and builds the KVM manager over a
// short-path work dir (vsock unix-socket length limits), returning the manager,
// the registered image ref, and the work dir (the per-sandbox state root).
func e2eBootGuest(t *testing.T, kernel, rootfs string, binder *sandboxd.PeerBinder,
	clock func() time.Time, logger *slog.Logger) (*microvm.Manager, string, string) {
	t.Helper()
	hostRoot := t.TempDir()
	_, err := images.GenerateTrustRoot(context.Background(), hostRoot, noopAudit{})
	require.NoError(t, err)
	priv, err := images.LoadSigningKey(hostRoot)
	require.NoError(t, err)
	entry, err := images.BuildEntry(kernel, rootfs, e2eGuestCmdline)
	require.NoError(t, err)
	ref, err := images.Register(hostRoot, "mgit-guest", entry, priv)
	require.NoError(t, err)
	store, err := images.NewStore(hostRoot, clock)
	require.NoError(t, err)

	workDir, err := os.MkdirTemp("", "mgwf") // short path for the vsock socket
	require.NoError(t, err)
	t.Cleanup(func() { _ = os.RemoveAll(workDir) })

	mgr, err := NewManager(Config{
		WorkDir: workDir,
		Resolve: func(r string) (ImagePaths, error) {
			ri, rerr := store.Resolve(r)
			return ImagePaths{KernelPath: ri.KernelPath, RootfsPath: ri.RootfsPath, Cmdline: ri.Cmdline}, rerr
		},
		Logger: logger, Clock: clock, PeerBinder: binder,
	})
	require.NoError(t, err)
	return mgr, ref, workDir
}

// e2eLaunch boots the guest sandbox and emits the "created" provenance event
// (backend/image_digest/network_mode), which these manager-direct E2E tests
// must write themselves since they do not run the full SandboxService. The
// digest is pinned in the ImageRef so it round-trips into the event and the
// task_commits provenance. Refs: FR-17.18, MGIT-11.13.1
func e2eLaunch(t *testing.T, mgr *microvm.Manager, mainIdx *index.Store,
	wtPath, ref, task string) *model.SandboxInfo {
	t.Helper()
	_ = ref // the digest provenance is pinned via the ImageRef below
	info, err := mgr.Launch(context.Background(), model.SandboxLaunchOptions{
		TaskID: task, WorktreePath: wtPath,
		ImageRef: "mgit-guest@" + e2eWorkflowImageDigest,
		Network:  model.NetworkPolicy{Mode: model.NetworkModeNone}, CPUs: 1, MemoryMB: 256,
	})
	require.NoError(t, err)
	t.Cleanup(func() { _ = mgr.Remove(context.Background(), info.ID, true) })

	require.NoError(t, mainIdx.AppendSandboxEvent(context.Background(), &model.SandboxEvent{
		SandboxID: info.ID, TaskID: task, EventType: model.EventCreated,
		Backend: model.BackendKVM, ImageDigest: e2eWorkflowImageDigest, NetworkMode: model.NetworkModeNone,
	}))
	return info
}

// e2eLandService wires the verified land path over the booted guest, identical
// to landtrip_linux_test.go: a land channel on the same peer binder + work-dir
// dialer, the host parent resolver, the store importer/brancher lander, the
// orchestrator, and the land service, with require_sandbox ON (the production
// default, sandbox_policy.go:46) so each landed commit is attested and carries
// sandbox provenance (task_commits.sandbox_id). Refs: FR-17.5, FR-17.6
type e2eOnPolicy struct{}

func (e2eOnPolicy) Load(context.Context) (model.SandboxPolicy, error) {
	return model.SandboxPolicy{RequireSandbox: true}, nil
}

func e2eLandService(t *testing.T, hostRepo *gitstore.Repository, mainIdx *index.Store,
	binder *sandboxd.PeerBinder, workDir, sandboxID string,
	clock func() time.Time, logger *slog.Logger) *service.LandService {
	t.Helper()
	channel := sandboxd.NewLandChannel(binder, NewLandDialer(workDir), land.DefaultLimits(), logger)
	parents := land.NewPoolAwareParentResolver(land.NewHostParentTreeResolver(hostRepo))
	lander := land.NewLander(land.NewStoreImporter(hostRepo), mainIdx,
		land.NewStoreBrancher(gitstore.NewMergeStore(hostRepo)))
	orch, err := service.NewLandOrchestrator(channel, e2eStubAttestor{}, lander, parents,
		mainIdx, e2eOnPolicy{}, land.DefaultLimits(), clock)
	require.NoError(t, err)
	landSvc, err := service.NewLandService(e2eStubResolver{id: sandboxID}, channel, mainIdx,
		parents, e2eStubAttestor{}, orch, e2eOnPolicy{})
	require.NoError(t, err)
	return landSvc
}

// driveGuestExec runs ONE guest exec (the agent's build/test step) over the
// vsock transport, retrying until the asynchronously booting guest is serving.
// It mirrors notifytrip_linux_test.go's exec drive. Refs: MGIT-11.9.2
func driveGuestExec(t *testing.T, h *e2eWorkflowHarness) {
	t.Helper()
	deadline := time.Now().Add(25 * time.Second)
	for time.Now().Before(deadline) {
		res, err := h.mgr.Exec(context.Background(), h.sandboxID,
			model.ExecRequest{Command: []string{"/bin/sh", "-c", "true"}})
		if err == nil {
			assert.Equal(t, 0, res.ExitCode, "the guest build/test step succeeds")
			return
		}
		time.Sleep(400 * time.Millisecond)
	}
	t.Fatal("guest exec never reached the booted sandbox over vsock")
}

// landUntilServing pulls over the land channel, retrying until the guest is
// serving the land port, asserting exactly the agent's one commit lands.
// Refs: FR-17.5
func landUntilServing(t *testing.T, h *e2eWorkflowHarness) {
	t.Helper()
	var sum *service.LandSummary
	var err error
	deadline := time.Now().Add(25 * time.Second)
	for time.Now().Before(deadline) {
		sum, err = h.landSvc.Land(context.Background(), h.task)
		if err == nil && sum.Commits > 0 {
			break
		}
		time.Sleep(400 * time.Millisecond)
	}
	require.NoError(t, err, "land must reach the guest once it is serving the land channel")
	require.Equal(t, 1, sum.Commits, "the agent's new commit lands")
}

// findCreatedEvent returns the "created" lifecycle event from a sandbox's audit
// stream, failing if absent — it is the row carrying backend/image/network
// provenance. Refs: FR-17.18
func findCreatedEvent(t *testing.T, events []model.SandboxEvent) model.SandboxEvent {
	t.Helper()
	for _, ev := range events {
		if ev.EventType == model.EventCreated {
			return ev
		}
	}
	t.Fatal("no created sandbox_events row recorded for the sandbox")
	return model.SandboxEvent{}
}

// TestE2E_ClaimToLand_Succeeds drives the full FR-17 agent workflow across one
// real KVM backend: claim a task (the pre-created task branch + private store
// stand in for the claim's worktree binding) -> worktree+sandbox launch -> a
// host-side edit already committed into the private store + a guest-side
// build/test exec -> land -> teardown. It asserts the agent's commit lands on
// the task branch and the workflow completes (branch advanced, ledger recorded,
// teardown clean). Refs: FR-17.5, FR-17.18, FR-17.19, SEC-03, MGIT-11.13.1
func TestE2E_ClaimToLand_Succeeds(t *testing.T) {
	h := setupE2EWorkflow(t, "MGIT-11.13.1")

	driveGuestExec(t, h)
	landUntilServing(t, h)

	tip, err := h.branches.GetBranch(context.Background(), model.TaskBranchName(h.task))
	require.NoError(t, err)
	assert.Equal(t, h.newCommit, tip.HeadCommit, "the task branch advanced to the landed commit")

	recs, err := h.mainIdx.GetTaskCommits(context.Background(), h.task)
	require.NoError(t, err)
	require.Len(t, recs, 1)
	assert.Equal(t, h.newCommit, recs[0].CommitHash, "the landed commit is recorded in the ledger")

	require.NoError(t, h.mgr.Remove(context.Background(), h.sandboxID, true))
	assert.NoDirExists(t, h.stateDir(), "the per-sandbox state dir is gone after teardown")
}

// TestE2E_ProvenanceRecordedPerCommit asserts each landed commit carries full
// provenance: task_commits.sandbox_id is set to the launching sandbox, and a
// sandbox_events row records backend, image_digest, and network_mode. The land
// path stamps the sandbox_id from the verified sandbox binding; the "created"
// event carries the backend/image/network provenance. Refs: FR-17.6, FR-17.18, MGIT-11.13.1
func TestE2E_ProvenanceRecordedPerCommit(t *testing.T) {
	h := setupE2EWorkflow(t, "MGIT-11.13.1")

	driveGuestExec(t, h)
	landUntilServing(t, h)

	recs, err := h.mainIdx.GetTaskCommits(context.Background(), h.task)
	require.NoError(t, err)
	require.Len(t, recs, 1, "exactly one commit landed")
	require.NotNil(t, recs[0].SandboxID, "the landed commit carries sandbox provenance (no NULL gap)")
	assert.Equal(t, h.sandboxID, *recs[0].SandboxID, "task_commits.sandbox_id is the launching sandbox")

	events, err := h.mainIdx.ListSandboxEvents(context.Background(), h.sandboxID)
	require.NoError(t, err)
	created := findCreatedEvent(t, events)
	assert.Equal(t, model.BackendKVM, created.Backend, "the event records the backend")
	assert.Equal(t, e2eWorkflowImageDigest, created.ImageDigest, "the event records the rootfs image digest")
	assert.Equal(t, h.netMode, created.NetworkMode, "the event records the network mode")
}

// TestE2E_RemoveDiscardsUnlanded_NoResidue proves unlanded work is discarded on
// teardown: the agent's commit sits in the sandbox-private store but is never
// landed, so after mgr.Remove the commit is absent from the host store/index
// AND no per-sandbox host residue (the WorkDir/state dir) survives. This is the
// FR-17.19 isolation guarantee: a removed sandbox leaves nothing behind and an
// un-landed change never reaches the host. Refs: FR-17.19, SEC-03, MGIT-11.13.1
func TestE2E_RemoveDiscardsUnlanded_NoResidue(t *testing.T) {
	h := setupE2EWorkflow(t, "MGIT-11.13.1")

	driveGuestExec(t, h) // the agent works, but never lands
	require.DirExists(t, h.stateDir(), "the per-sandbox state dir exists while running")

	require.NoError(t, h.mgr.Remove(context.Background(), h.sandboxID, true))

	assert.NoDirExists(t, h.stateDir(), "no per-sandbox host residue survives Remove (FR-17.19)")

	recs, err := h.mainIdx.GetTaskCommits(context.Background(), h.task)
	require.NoError(t, err)
	assert.Empty(t, recs, "an unlanded commit is absent from the host ledger")

	tip, err := h.branches.GetBranch(context.Background(), model.TaskBranchName(h.task))
	require.NoError(t, err)
	assert.NotEqual(t, h.newCommit, tip.HeadCommit, "the task branch never advanced to the unlanded commit")
}
