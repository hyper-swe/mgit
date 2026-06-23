//go:build darwin && cgo

// Full-stack land round-trip on macOS (Virtualization.framework) over the
// vzf host LAND dialer, on the SEC-03 .mgit model: boot a Linux guest under
// Apple's framework whose mounted worktree carries a PRIVATE, sandbox-local
// .mgit store with a new commit, then pull that commit over the guest land
// channel (the dialer's VZVirtioSocketDevice.Connect on the live VM, land
// port), verify it host-side, and atomically import + fast-forward the host
// task branch. It is the macOS counterpart of firecracker's
// landtrip_linux_test.go, proving guest land server -> host pull (over the
// framework vsock) -> verify -> import -> fast-forward end to end on vzf.
//
// vzf runs a LINUX guest under Virtualization.framework, so it reuses the
// SAME guest image as the Linux backend (there is no macOS guest image); the
// guest serves <worktree>/.mgit (a bare store), never a .git, and never the
// host shared store. Gated, like the exec round-trip, on the
// com.apple.security.virtualization entitlement (a signed test binary) and a
// guest image via MGIT_E2E_VZF_KERNEL + MGIT_E2E_GUEST_ROOTFS, so without
// them it skips rather than fails. Refs: FR-17.5, FR-17.16, SEC-03, MGIT-13.1.1, MGIT-11.13
package vzf

import (
	"context"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
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
	"github.com/hyper-swe/mgit/internal/sandboxd/land"
	"github.com/hyper-swe/mgit/internal/sandboxd/provision"
	"github.com/hyper-swe/mgit/internal/service"
	gitstore "github.com/hyper-swe/mgit/internal/store/git"
	"github.com/hyper-swe/mgit/internal/store/index"
)

// e2eLandResolver resolves the bound sandbox by its launched id.
type e2eLandResolver struct{ id string }

func (r e2eLandResolver) Status(context.Context, string) (*model.SandboxInfo, error) {
	return &model.SandboxInfo{ID: r.id}, nil
}

// e2eLandOffPolicy disables require_sandbox so the round-trip needs no
// attestation (the land import path itself is what this proves).
type e2eLandOffPolicy struct{}

func (e2eLandOffPolicy) Load(context.Context) (model.SandboxPolicy, error) {
	return model.SandboxPolicy{RequireSandbox: false}, nil
}

// e2eLandAttestor is never consulted under require_sandbox=off; it satisfies
// both the orchestrator's verifier and the land service's attestor.
type e2eLandAttestor struct{}

func (e2eLandAttestor) Attest(_ context.Context, sandboxID, commitHash, contentHash string) (*model.Attestation, error) {
	return &model.Attestation{SandboxID: sandboxID, CommitHash: commitHash, ContentHash: contentHash}, nil
}
func (e2eLandAttestor) Verify(context.Context, *model.Attestation) error { return nil }

// TestE2E_VZF_Land_RealGuest_RoundTrip proves the whole land stack on real
// Virtualization.framework with the SEC-03 .mgit model: the host provisions a
// private store (task base only), the agent's new commit goes into that
// private store, the worktree image delivers worktree files + the private
// .mgit, and the guest serves the private store's HEAD over the land channel.
// The host pulls over the vzf land dialer (VZVirtioSocketDevice.Connect on the
// live VM, land port), verifies (dual-hash + tree binding), imports, and
// fast-forwards the task branch. The guest holds no key.
func TestE2E_VZF_Land_RealGuest_RoundTrip(t *testing.T) {
	kernel := os.Getenv("MGIT_E2E_VZF_KERNEL")
	rootfs := os.Getenv("MGIT_E2E_GUEST_ROOTFS")
	if kernel == "" || rootfs == "" {
		t.Skip("set MGIT_E2E_VZF_KERNEL and MGIT_E2E_GUEST_ROOTFS (a guest image serving mgit-guest on the land vsock port) to run the vzf land round-trip")
	}
	for _, p := range []string{kernel, rootfs} {
		if !fileExists(p) {
			t.Skipf("guest image %s absent", p)
		}
	}
	const task = "MGIT-13.1.1"
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

	// Materialize the worktree as PLAIN FILES (no store inside it), provision a
	// fresh private .mgit store seeded with the task base, and put the agent's
	// new commit into THAT private store — the .mgit the image delivers and the
	// guest serves over the land channel. Refs: SEC-03
	wtPath := filepath.Join(t.TempDir(), "repo", "worktrees", "task-a")
	require.NoError(t, gitstore.NewWorktreeStore(hostRepo).MaterializeBranchTo(context.Background(), model.TaskBranchName(task), wtPath))

	prov, err := provision.NewStoreProvisioner(hostRepoRoot)
	require.NoError(t, err)
	privDir := filepath.Join(wtPath, ".mgit")
	_, err = prov.Provision(task, privDir)
	require.NoError(t, err)
	newCommit := commitIntoVZFPrivateStore(t, privDir, task)

	mainIdx, err := index.New(filepath.Join(hostRepoRoot, ".mgit", "index.db"), clock)
	require.NoError(t, err)
	t.Cleanup(func() { _ = mainIdx.Close() })

	// Boot the guest through the vzf manager and capture the land dialer bound
	// to the same live-VM registry the hypervisor publishes into. No
	// StoreProvisioner: deliver the pre-built worktree+.mgit directly so the
	// guest serves the store carrying the agent's commit (the guest cannot run
	// mgit to commit inside the VM, so it is injected host-side). The SEC-03
	// provisioner+staging DELIVERY is proven by worktree_share_darwin_test.go.
	workDir := t.TempDir()
	binder := sandboxd.NewPeerBinder(logger)
	mgr, landDialer, err := NewManagerWithLand(Config{
		WorkDir: workDir,
		Resolve: func(string) (ImagePaths, error) {
			return ImagePaths{KernelPath: kernel, RootfsPath: rootfs, Cmdline: "console=hvc0 root=/dev/vda ro"}, nil
		},
		Logger: logger, Clock: clock, PeerBinder: binder,
	})
	require.NoError(t, err)

	info, err := mgr.Launch(context.Background(), model.SandboxLaunchOptions{
		TaskID: task, WorktreePath: wtPath,
		ImageRef: "mgit-guest@sha256:" + strings.Repeat("a", 64),
		Network:  model.NetworkPolicy{Mode: model.NetworkModeNone}, CPUs: 1, MemoryMB: 512,
	})
	if err != nil && strings.Contains(err.Error(), "com.apple.security.virtualization") {
		t.Skipf("test binary lacks the virtualization entitlement: %v", err)
	}
	require.NoError(t, err)
	t.Cleanup(func() { _ = mgr.Remove(context.Background(), info.ID, true) })

	// Wire the verified land path over the booted guest, using the vzf land
	// dialer (VZVirtioSocketDevice.Connect on the live VM, land port).
	channel := sandboxd.NewLandChannel(binder, landDialer, land.DefaultLimits(), logger)
	parents := land.NewPoolAwareParentResolver(land.NewHostParentTreeResolver(hostRepo))
	lander := land.NewLander(land.NewStoreImporter(hostRepo), mainIdx, land.NewStoreBrancher(ms))
	orch, err := service.NewLandOrchestrator(channel, e2eLandAttestor{}, lander, parents,
		mainIdx, e2eLandOffPolicy{}, land.DefaultLimits(), clock)
	require.NoError(t, err)
	landSvc, err := service.NewLandService(e2eLandResolver{id: info.ID}, channel, mainIdx,
		parents, e2eLandAttestor{}, orch, e2eLandOffPolicy{})
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
	require.NoError(t, err, "land must reach the guest over the framework vsock once it is serving the land channel")
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

// commitIntoVZFPrivateStore adds one new commit (the agent's work) on top of
// the private store's seeded base, on the task branch, and returns its hash.
// It commits via plumbing into the bare .mgit store the SEC-03 model delivers
// — the same store the guest serves over the land channel. Refs: SEC-03
func commitIntoVZFPrivateStore(t *testing.T, privDir, task string) string {
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
