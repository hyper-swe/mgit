//go:build linux

// Full-stack land round-trip against a real mgit-guest image, on the .mgit
// model (SEC-03): boot a guest whose mounted worktree carries a PRIVATE,
// sandbox-local .mgit store with a new commit, then pull that commit over the
// guest land channel, verify it host-side, and atomically import + fast-forward
// the host task branch — exercising guest land server -> host pull -> verify ->
// import -> fast-forward end to end. The guest serves <worktree>/.mgit (a bare
// store), never a .git, and never the host shared store. Gated, like the exec
// round-trip, on a prebuilt mgit-guest rootfs (which must include the land
// listener) via MGIT_E2E_GUEST_ROOTFS. Refs: FR-17.5, SEC-03, MGIT-11.6.8
package firecracker

import (
	"context"
	"log/slog"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/cache"
	"github.com/go-git/go-git/v5/plumbing/object"
	"github.com/go-git/go-git/v5/storage/filesystem"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/go-git/go-billy/v5/osfs"

	"github.com/hyper-swe/mgit/internal/model"
	"github.com/hyper-swe/mgit/internal/sandboxd"
	"github.com/hyper-swe/mgit/internal/sandboxd/images"
	"github.com/hyper-swe/mgit/internal/sandboxd/land"
	"github.com/hyper-swe/mgit/internal/sandboxd/provision"
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

// TestE2E_Land_RealGuest_RoundTrip proves the whole land stack on real KVM with
// the SEC-03 .mgit model: the host provisions a private store (task base only),
// the agent's new commit goes into that private store, the worktree image
// delivers worktree files + the private .mgit, and the guest serves the private
// store's HEAD over the land channel. The host pulls, verifies (dual-hash + tree
// binding), imports, and fast-forwards the task branch. The guest holds no key.
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

	// Host shared repo + main index. The task branch is pre-created at the
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

	// Materialize the worktree as PLAIN FILES (no store inside it), then
	// provision a fresh private .mgit store seeded with the task base, and put
	// the agent's new commit into THAT private store — the .mgit the image
	// delivers and the guest serves. Refs: SEC-03
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

	// Register the guest image and boot it. This test proves the land STACK on
	// the .mgit model, so it delivers the pre-built private store directly (the
	// worktree, with its .mgit carrying the agent's commit, is packed as-is):
	// the guest cannot run mgit to commit inside the VM, so the new commit must
	// be injected host-side into the store the guest will serve. The SEC-03
	// provisioner+staging DELIVERY (fresh base-only store, shared-store
	// exclusion) is proven separately by the hostile-guest e2e. Refs: SEC-03
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
		// No StoreProvisioner: deliver the pre-built worktree+.mgit directly so
		// the guest serves the store carrying the agent's commit (see above).
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

// commitIntoPrivateStore adds one new commit (the agent's work) on top of the
// private store's seeded base, on the task branch, and returns its hash. It
// commits via plumbing into the bare .mgit store the SEC-03 model delivers —
// the same store the guest commits into and ServeLandHead serves. Refs: SEC-03
func commitIntoPrivateStore(t *testing.T, privDir, task string) string {
	t.Helper()
	st := filesystem.NewStorage(osfs.New(privDir), cache.NewObjectLRUDefault())
	branch := plumbing.NewBranchReferenceName(model.TaskBranchName(task))
	parent, err := st.Reference(branch)
	require.NoError(t, err)

	blob := st.NewEncodedObject()
	blob.SetType(plumbing.BlobObject)
	bw, err := blob.Writer()
	require.NoError(t, err)
	_, _ = bw.Write([]byte("agent work"))
	require.NoError(t, bw.Close())
	blobHash, err := st.SetEncodedObject(blob)
	require.NoError(t, err)

	tree := st.NewEncodedObject()
	require.NoError(t, (&object.Tree{Entries: []object.TreeEntry{
		{Name: "work.txt", Mode: 0o100644, Hash: blobHash},
	}}).Encode(tree))
	treeHash, err := st.SetEncodedObject(tree)
	require.NoError(t, err)

	sig := object.Signature{Name: "agent", Email: "agent@mgit", When: time.Now().UTC()}
	commit := st.NewEncodedObject()
	require.NoError(t, (&object.Commit{
		Author: sig, Committer: sig, Message: "feat: agent work",
		TreeHash: treeHash, ParentHashes: []plumbing.Hash{parent.Hash()},
	}).Encode(commit))
	ch, err := st.SetEncodedObject(commit)
	require.NoError(t, err)
	require.NoError(t, st.SetReference(plumbing.NewHashReference(branch, ch)))
	return ch.String()
}
