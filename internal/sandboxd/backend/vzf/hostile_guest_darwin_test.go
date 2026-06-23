//go:build darwin && cgo

// Hostile-guest SEC-03 round-trip for vzf against a real Virtualization.framework
// guest: the guest is the attacker. It proves the staged-share quarantine holds
// against a guest that probes its filesystem — the host SHARED .mgit is not
// reachable, another task's objects never reach the private store, an escaping
// worktree symlink is REJECTED on delivery (launch fails closed), and land is
// the only private->shared bridge. The vzf counterpart of firecracker's
// hostile_guest_linux_test.go.
//
// Gated like the other vzf e2e: it needs the com.apple.security.virtualization
// entitlement (a signed test binary) and a guest image that serves mgit-guest
// and carries grep/find/head/test applets, so without them it skips. Refs:
// SEC-03, FR-17.3, FR-17.5, MGIT-11.6.9
package vzf

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
	"github.com/hyper-swe/mgit/internal/sandboxd/backend/microvm"
	"github.com/hyper-swe/mgit/internal/sandboxd/provision"
	"github.com/hyper-swe/mgit/internal/sandboxd/staging"
	gitstore "github.com/hyper-swe/mgit/internal/store/git"
)

const hostileTask = "MGIT-11.6.9"

// vzfHostileFixture is the host-side setup: a shared repo whose task branch has
// a base, another task's SECRET object in the shared store, the materialized
// worktree, and a wired provisioner.
type vzfHostileFixture struct {
	repoRoot string
	task     string
	wtPath   string
	prov     *provision.StoreProvisioner
	secret   string
}

func setupVZFHostileFixture(t *testing.T) vzfHostileFixture {
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

	wtPath := filepath.Join(t.TempDir(), "worktrees", "task-a")
	require.NoError(t, gitstore.NewWorktreeStore(repo).MaterializeBranchTo(
		context.Background(), model.TaskBranchName(hostileTask), wtPath))
	require.NoError(t, os.WriteFile(filepath.Join(wtPath, "marker.txt"), []byte("work area"), 0o600))

	prov, err := provision.NewStoreProvisioner(repoRoot)
	require.NoError(t, err)
	return vzfHostileFixture{repoRoot: repoRoot, task: hostileTask, wtPath: wtPath, prov: prov, secret: secret}
}

// vzfBootGuest boots a quarantined vzf guest, skipping if the entitlement or
// images are absent.
func vzfBootGuest(t *testing.T, fx vzfHostileFixture, kernel, rootfs string) (*microvm.Manager, string) {
	t.Helper()
	mgr, err := NewManager(Config{
		WorkDir: t.TempDir(),
		Resolve: func(string) (ImagePaths, error) {
			return ImagePaths{KernelPath: kernel, RootfsPath: rootfs, Cmdline: "console=hvc0 root=/dev/vda ro"}, nil
		},
		Logger:           slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError})),
		Clock:            func() time.Time { return time.Now().UTC() },
		StoreProvisioner: fx.prov,
		SensitivePaths:   model.DefaultSandboxPolicy().SensitivePaths,
	})
	require.NoError(t, err)
	info, err := mgr.Launch(context.Background(), model.SandboxLaunchOptions{
		TaskID: fx.task, WorktreePath: fx.wtPath,
		ImageRef: "mgit-guest@sha256:" + strings.Repeat("a", 64),
		Network:  model.NetworkPolicy{Mode: model.NetworkModeNone}, CPUs: 1, MemoryMB: 512,
	})
	if err != nil && strings.Contains(err.Error(), "com.apple.security.virtualization") {
		t.Skipf("test binary lacks the virtualization entitlement: %v", err)
	}
	require.NoError(t, err)
	t.Cleanup(func() { _ = mgr.Remove(context.Background(), info.ID, true) })
	return mgr, info.ID
}

// vzfGuestExec retries an exec until the guest serves vsock, then returns it.
func vzfGuestExec(t *testing.T, mgr *microvm.Manager, id, shellCmd string) *model.ExecResult {
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

// requireVZFImages skips unless both a kernel and a guest rootfs are supplied.
func requireVZFImages(t *testing.T) (kernel, rootfs string) {
	t.Helper()
	kernel = os.Getenv("MGIT_E2E_VZF_KERNEL")
	rootfs = os.Getenv("MGIT_E2E_GUEST_ROOTFS")
	if kernel == "" || rootfs == "" {
		t.Skip("set MGIT_E2E_VZF_KERNEL and MGIT_E2E_GUEST_ROOTFS (a guest image with the land listener + grep/find/head/test applets) to run the vzf hostile-guest e2e")
	}
	for _, p := range []string{kernel, rootfs} {
		if !fileExists(p) {
			t.Skipf("guest image %s absent", p)
		}
	}
	return kernel, rootfs
}

// TestE2E_VZF_HostileGuest_SharedStoreUnreachable proves a compromised vzf guest
// cannot reach the host shared store nor another task's objects, seeing only its
// private store. The probe is BOUNDED to the worktree subtree (never `/`, which
// would hang on /proc,/sys,/dev) with a positive control. Refs: SEC-03 T6
func TestE2E_VZF_HostileGuest_SharedStoreUnreachable(t *testing.T) {
	kernel, rootfs := requireVZFImages(t)
	fx := setupVZFHostileFixture(t)
	mgr, id := vzfBootGuest(t, fx, kernel, rootfs)

	res := vzfGuestExec(t, mgr, id,
		"test -d "+filepath.Join(fx.wtPath, ".mgit")+" && echo PRIVATE_OK; "+
			"test -e "+filepath.Join(fx.repoRoot, ".mgit")+" && echo SHARED_LEAK || echo SHARED_ABSENT")
	out := string(res.Stdout)
	assert.Contains(t, out, "PRIVATE_OK", "the guest's private .mgit store is present")
	assert.Contains(t, out, "SHARED_ABSENT", "the host shared .mgit store is not reachable from the guest")
	assert.NotContains(t, out, "SHARED_LEAK")

	// The foreign secret appears NOWHERE the guest can reach. The grep is scoped
	// to the worktree subtree (which contains the private .mgit) — grepping `/`
	// would hang on virtual filesystems. A positive control proves grep works.
	res = vzfGuestExec(t, mgr, id,
		"grep -rl 'work area' "+fx.wtPath+" 2>/dev/null | head -n1; echo ---; "+
			"grep -rl 'OTHER-TASK-SECRET' "+fx.wtPath+" 2>/dev/null | head -n1 || true")
	parts := strings.SplitN(string(res.Stdout), "---", 2)
	require.Len(t, parts, 2, "probe produced both halves")
	assert.NotEmpty(t, strings.TrimSpace(parts[0]),
		"positive control: grep finds the worktree marker (proves grep works in the guest)")
	assert.NotContains(t, parts[1], "OTHER-TASK-SECRET",
		"another task's object must never be reachable from the guest (SEC-03 T6)")
}

// TestE2E_VZF_HostileGuest_EscapingSymlinkRejected proves an escaping worktree
// symlink fails the launch CLOSED: the guest never boots. Refs: SEC-03, F-A/NEW-2
func TestE2E_VZF_HostileGuest_EscapingSymlinkRejected(t *testing.T) {
	kernel, rootfs := requireVZFImages(t)
	fx := setupVZFHostileFixture(t)
	outside := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(outside, "host-secret"), []byte("host secret"), 0o600))
	require.NoError(t, os.Symlink(filepath.Join(outside, "host-secret"), filepath.Join(fx.wtPath, "escape")))

	mgr, err := NewManager(Config{
		WorkDir: t.TempDir(),
		Resolve: func(string) (ImagePaths, error) {
			return ImagePaths{KernelPath: kernel, RootfsPath: rootfs, Cmdline: "console=hvc0 root=/dev/vda ro"}, nil
		},
		Logger:           slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError})),
		Clock:            func() time.Time { return time.Now().UTC() },
		StoreProvisioner: fx.prov,
		SensitivePaths:   model.DefaultSandboxPolicy().SensitivePaths,
	})
	require.NoError(t, err)
	_, err = mgr.Launch(context.Background(), model.SandboxLaunchOptions{
		TaskID: fx.task, WorktreePath: fx.wtPath,
		ImageRef: "mgit-guest@sha256:" + strings.Repeat("a", 64),
		Network:  model.NetworkPolicy{Mode: model.NetworkModeNone}, CPUs: 1, MemoryMB: 512,
	})
	require.ErrorIs(t, err, staging.ErrSymlinkEscape,
		"the launch must fail CLOSED with the symlink-escape error, not an incidental failure")
}

// TestE2E_VZF_HostileGuest_LandIsOnlyBridge proves land is the only
// private->shared path: a booted guest that never lands leaves the host shared
// store's task branch unchanged.
func TestE2E_VZF_HostileGuest_LandIsOnlyBridge(t *testing.T) {
	kernel, rootfs := requireVZFImages(t)
	fx := setupVZFHostileFixture(t)

	shared := filesystem.NewStorage(osfs.New(filepath.Join(fx.repoRoot, ".mgit")), cache.NewObjectLRUDefault())
	before, err := shared.Reference(plumbing.NewBranchReferenceName(model.TaskBranchName(fx.task)))
	require.NoError(t, err)

	mgr, id := vzfBootGuest(t, fx, kernel, rootfs)
	vzfGuestExec(t, mgr, id, "cd "+fx.wtPath+" && echo more > extra.txt") // a guest-side write

	repo, err := gogit.Open(shared, nil)
	require.NoError(t, err)
	after, err := repo.Reference(plumbing.NewBranchReferenceName(model.TaskBranchName(fx.task)), false)
	require.NoError(t, err)
	assert.Equal(t, before.Hash(), after.Hash(),
		"without land, the host shared store's task branch is untouched (land is the only bridge)")
}
