//go:build linux

// Hostile-guest SEC-03 round-trip against a real mgit-guest image: the guest
// is the attacker. It proves the host-side quarantine guarantees hold against
// a guest that actively probes its filesystem:
//
//   - the host SHARED .mgit store is not reachable from inside the guest;
//   - another task's objects (present in the shared store) never reach the
//     guest's private store or its filesystem;
//   - a worktree symlink whose target escapes the worktree is REJECTED on
//     delivery, so the launch fails closed and no guest boots;
//   - `mgit sandbox land` is the only private->shared bridge: a booted guest
//     that never lands leaves the host shared store untouched.
//
// KVM + root gated, like the other e2e suites, on MGIT_E2E_GUEST_ROOTFS (and a
// usable /dev/kvm). Cannot run without a real guest image; skips otherwise.
// Refs: SEC-03, FR-17.3, FR-17.5, MGIT-11.6.8
package firecracker

import (
	"context"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	gogit "github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/cache"
	"github.com/go-git/go-git/v5/plumbing/object"
	"github.com/go-git/go-git/v5/storage/filesystem"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/go-git/go-billy/v5/osfs"

	"github.com/hyper-swe/mgit/internal/model"
	"github.com/hyper-swe/mgit/internal/sandboxd"
	"github.com/hyper-swe/mgit/internal/sandboxd/backend/microvm"
	"github.com/hyper-swe/mgit/internal/sandboxd/images"
	"github.com/hyper-swe/mgit/internal/sandboxd/provision"
	gitstore "github.com/hyper-swe/mgit/internal/store/git"
)

// hostileFixture is the host-side setup shared by the hostile-guest sub-tests:
// a shared repo whose task branch has a base, plus another task's SECRET object
// in the shared store, the materialized worktree, and a wired provisioner.
type hostileFixture struct {
	repoRoot   string
	task       string
	wtPath     string
	prov       *provision.StoreProvisioner
	secretBlob string // content of the other task's object, never to appear in the guest
}

const hostileTask = "MGIT-11.6.8"

// setupHostileFixture builds the shared store with a task base AND another
// task's secret object, then materializes the worktree and provisions the
// private store seeded with the task base only.
func setupHostileFixture(t *testing.T) hostileFixture {
	t.Helper()
	clock := func() time.Time { return time.Now().UTC() }
	repoRoot := t.TempDir()
	repo, err := gitstore.Init(repoRoot, clock)
	require.NoError(t, err)
	t.Cleanup(func() { _ = repo.Close() })
	base, err := repo.Head()
	require.NoError(t, err)
	require.NoError(t, gitstore.NewBranchStore(repo).CreateBranch(context.Background(),
		&model.Branch{Name: model.TaskBranchName(hostileTask), HeadCommit: base}))

	// Plant another task's SECRET object in the shared store, reachable only
	// from a foreign branch — it must NEVER reach the guest (SEC-03 T6).
	secret := "OTHER-TASK-SECRET-" + strings.Repeat("Z", 16)
	shared := filesystem.NewStorage(osfs.New(filepath.Join(repoRoot, ".mgit")), cache.NewObjectLRUDefault())
	sb := shared.NewEncodedObject()
	sb.SetType(plumbing.BlobObject)
	w, err := sb.Writer()
	require.NoError(t, err)
	_, _ = w.Write([]byte(secret))
	require.NoError(t, w.Close())
	secretHash, err := shared.SetEncodedObject(sb)
	require.NoError(t, err)
	otherTree := shared.NewEncodedObject()
	require.NoError(t, (&object.Tree{Entries: []object.TreeEntry{
		{Name: "secret.txt", Mode: 0o100644, Hash: secretHash},
	}}).Encode(otherTree))
	otherTreeHash, err := shared.SetEncodedObject(otherTree)
	require.NoError(t, err)
	sig := object.Signature{Name: "x", Email: "x@mgit", When: time.Now().UTC()}
	oc := shared.NewEncodedObject()
	require.NoError(t, (&object.Commit{Author: sig, Committer: sig, Message: "secret", TreeHash: otherTreeHash}).Encode(oc))
	otherCH, err := shared.SetEncodedObject(oc)
	require.NoError(t, err)
	require.NoError(t, shared.SetReference(
		plumbing.NewHashReference(plumbing.NewBranchReferenceName("task/OTHER-9.9"), otherCH)))

	wtPath := filepath.Join(t.TempDir(), "repo", "worktrees", "task-a")
	require.NoError(t, gitstore.NewWorktreeStore(repo).MaterializeBranchTo(
		context.Background(), model.TaskBranchName(hostileTask), wtPath))
	require.NoError(t, os.WriteFile(filepath.Join(wtPath, "marker.txt"), []byte("work area"), 0o600))

	prov, err := provision.NewStoreProvisioner(repoRoot)
	require.NoError(t, err)
	return hostileFixture{repoRoot: repoRoot, task: hostileTask, wtPath: wtPath, prov: prov, secretBlob: secret}
}

// bootGuest registers + boots a guest with the SEC-03 provisioner wired and
// returns a running manager + sandbox id.
func bootGuest(t *testing.T, fx hostileFixture, kernel, rootfs string) (*microvm.Manager, string) {
	t.Helper()
	clock := func() time.Time { return time.Now().UTC() }
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))

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

	workDir, err := os.MkdirTemp("", "mghostile")
	require.NoError(t, err)
	t.Cleanup(func() { _ = os.RemoveAll(workDir) })

	mgr, err := NewManager(Config{
		WorkDir: workDir,
		Resolve: func(r string) (ImagePaths, error) {
			ri, rerr := store.Resolve(r)
			return ImagePaths{KernelPath: ri.KernelPath, RootfsPath: ri.RootfsPath, Cmdline: ri.Cmdline}, rerr
		},
		Logger: logger, Clock: clock, PeerBinder: sandboxd.NewPeerBinder(logger),
		StoreProvisioner: fx.prov,
		SensitivePaths:   model.DefaultSandboxPolicy().SensitivePaths,
	})
	require.NoError(t, err)
	info, err := mgr.Launch(context.Background(), model.SandboxLaunchOptions{
		TaskID: fx.task, WorktreePath: fx.wtPath, ImageRef: ref,
		Network: model.NetworkPolicy{Mode: model.NetworkModeNone}, CPUs: 1, MemoryMB: 256,
	})
	require.NoError(t, err)
	t.Cleanup(func() { _ = mgr.Remove(context.Background(), info.ID, true) })
	return mgr, info.ID
}

// guestExec retries an exec until the guest is serving vsock, then returns it.
func guestExec(t *testing.T, mgr *microvm.Manager, id, shellCmd string) *model.ExecResult {
	t.Helper()
	var res *model.ExecResult
	var err error
	deadline := time.Now().Add(25 * time.Second)
	for time.Now().Before(deadline) {
		res, err = mgr.Exec(context.Background(), id, model.ExecRequest{Command: []string{"/bin/sh", "-c", shellCmd}})
		if err == nil {
			return res
		}
		time.Sleep(400 * time.Millisecond)
	}
	require.NoError(t, err, "exec must reach the guest once it is serving vsock")
	return res
}

// TestE2E_HostileGuest_SharedStoreUnreachable proves a compromised guest cannot
// reach the host shared store nor another task's objects, and only sees its
// private store. The guest actively probes its filesystem.
func TestE2E_HostileGuest_SharedStoreUnreachable(t *testing.T) {
	kernel, _ := requireKVM(t)
	rootfs := os.Getenv("MGIT_E2E_GUEST_ROOTFS")
	if rootfs == "" {
		t.Skip("set MGIT_E2E_GUEST_ROOTFS to a guest image (with the land listener) to run the hostile-guest e2e")
	}
	if !fileExists(rootfs) {
		t.Skipf("guest rootfs %s absent", rootfs)
	}
	fx := setupHostileFixture(t)
	mgr, id := bootGuest(t, fx, kernel, rootfs)

	// The guest's private store exists at <worktree>/.mgit; the host shared
	// store path (the project root's .mgit) does NOT exist inside the guest.
	res := guestExec(t, mgr, id,
		"test -d "+filepath.Join(fx.wtPath, ".mgit")+" && echo PRIVATE_OK; "+
			"test -e "+filepath.Join(fx.repoRoot, ".mgit")+" && echo SHARED_LEAK || echo SHARED_ABSENT")
	out := string(res.Stdout)
	assert.Contains(t, out, "PRIVATE_OK", "the guest's private .mgit store is present")
	assert.Contains(t, out, "SHARED_ABSENT", "the host shared .mgit store is not reachable from the guest")
	assert.NotContains(t, out, "SHARED_LEAK")

	// The other task's secret content appears NOWHERE the guest can reach. The
	// grep is scoped to the worktree subtree — which CONTAINS the guest's
	// private .mgit store — because that (and its store) is the only place a
	// packed shared store or a cross-task object could surface; grepping `/`
	// would hang traversing /proc, /sys, and /dev virtual files. A POSITIVE
	// CONTROL proves the grep actually works (else a missing/broken grep would
	// make the negative result vacuously pass): the worktree marker MUST be
	// found, the foreign secret MUST NOT.
	res = guestExec(t, mgr, id,
		"grep -rl 'work area' "+fx.wtPath+" 2>/dev/null | head -n1; echo ---; "+
			"grep -rl 'OTHER-TASK-SECRET' "+fx.wtPath+" 2>/dev/null | head -n1 || true")
	parts := strings.SplitN(string(res.Stdout), "---", 2)
	require.Len(t, parts, 2, "probe produced both halves")
	assert.NotEmpty(t, strings.TrimSpace(parts[0]),
		"positive control: grep finds the worktree marker (proves grep works in the guest)")
	assert.NotContains(t, parts[1], "OTHER-TASK-SECRET",
		"another task's object must never be reachable from the guest (SEC-03 T6)")
}

// TestE2E_HostileGuest_EscapingSymlinkRejected proves a worktree symlink whose
// target escapes the worktree fails the launch CLOSED: the guest never boots,
// so it can never follow the link to a host path. Refs: SEC-03, F-A/NEW-2
func TestE2E_HostileGuest_EscapingSymlinkRejected(t *testing.T) {
	kernel, _ := requireKVM(t)
	rootfs := os.Getenv("MGIT_E2E_GUEST_ROOTFS")
	if rootfs == "" {
		t.Skip("set MGIT_E2E_GUEST_ROOTFS to a guest image to run the hostile-guest e2e")
	}
	if !fileExists(rootfs) {
		t.Skipf("guest rootfs %s absent", rootfs)
	}
	fx := setupHostileFixture(t)
	// Plant an escaping symlink in the worktree (points at a host path outside).
	outside := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(outside, "host-secret"), []byte("host secret"), 0o600))
	require.NoError(t, os.Symlink(filepath.Join(outside, "host-secret"), filepath.Join(fx.wtPath, "escape")))

	clock := func() time.Time { return time.Now().UTC() }
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
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
	workDir, err := os.MkdirTemp("", "mghostile")
	require.NoError(t, err)
	t.Cleanup(func() { _ = os.RemoveAll(workDir) })

	mgr, err := NewManager(Config{
		WorkDir: workDir,
		Resolve: func(r string) (ImagePaths, error) {
			ri, rerr := store.Resolve(r)
			return ImagePaths{KernelPath: ri.KernelPath, RootfsPath: ri.RootfsPath, Cmdline: ri.Cmdline}, rerr
		},
		Logger: logger, Clock: clock, PeerBinder: sandboxd.NewPeerBinder(logger),
		StoreProvisioner: fx.prov,
		SensitivePaths:   model.DefaultSandboxPolicy().SensitivePaths,
	})
	require.NoError(t, err)
	_, err = mgr.Launch(context.Background(), model.SandboxLaunchOptions{
		TaskID: fx.task, WorktreePath: fx.wtPath, ImageRef: ref,
		Network: model.NetworkPolicy{Mode: model.NetworkModeNone}, CPUs: 1, MemoryMB: 256,
	})
	require.ErrorIs(t, err, errSymlinkEscape,
		"the launch must fail CLOSED with the symlink-escape error, not an incidental failure")
}

// TestE2E_HostileGuest_LandIsOnlyBridge proves land is the only private->shared
// path: a booted guest that never lands leaves the host shared store's refs
// unchanged (the task branch still points at the base). The land round-trip
// itself is proven by TestE2E_Land_RealGuest_RoundTrip.
func TestE2E_HostileGuest_LandIsOnlyBridge(t *testing.T) {
	kernel, _ := requireKVM(t)
	rootfs := os.Getenv("MGIT_E2E_GUEST_ROOTFS")
	if rootfs == "" {
		t.Skip("set MGIT_E2E_GUEST_ROOTFS to a guest image to run the hostile-guest e2e")
	}
	if !fileExists(rootfs) {
		t.Skipf("guest rootfs %s absent", rootfs)
	}
	fx := setupHostileFixture(t)

	// Record the shared task branch tip before boot.
	shared := filesystem.NewStorage(osfs.New(filepath.Join(fx.repoRoot, ".mgit")), cache.NewObjectLRUDefault())
	before, err := shared.Reference(plumbing.NewBranchReferenceName(model.TaskBranchName(fx.task)))
	require.NoError(t, err)

	// Boot the guest and mutate its worktree (a filesystem write inside the
	// sandbox), but never land. This proves the NEGATIVE half — no guest-side
	// activity reaches the host shared store unless land is invoked. The
	// POSITIVE half (a real in-guest commit that DOES cross over via land) is
	// proven by TestE2E_Land_RealGuest_RoundTrip; a self-contained positive
	// proof here would require the mgit CLI inside the guest image (out of
	// scope for v1, where the guest image carries only mgit-guest + busybox).
	mgr, id := bootGuest(t, fx, kernel, rootfs)
	guestExec(t, mgr, id, "cd "+fx.wtPath+" && echo more > extra.txt") // a guest-side write

	// The shared store's task branch is unchanged: nothing crossed over without
	// land.
	repo, err := gogit.Open(shared, nil)
	require.NoError(t, err)
	after, err := repo.Reference(plumbing.NewBranchReferenceName(model.TaskBranchName(fx.task)), false)
	require.NoError(t, err)
	assert.Equal(t, before.Hash(), after.Hash(),
		"without land, the host shared store's task branch is untouched (land is the only bridge)")
}
