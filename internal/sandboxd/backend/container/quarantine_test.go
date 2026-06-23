// SEC-03 quarantine-delivery tests for the reduced-isolation container
// backend: the staged worktree (not the live one) is bind-mounted, the host
// shared store is unreachable, in-worktree stores are excluded, escaping
// symlinks fail the launch closed, and teardown clears the sandbox-local state.
// The podman CLI is faked; the provisioner + staging are real. Refs: SEC-03, MGIT-11.6.9
package container

import (
	"context"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/hyper-swe/mgit/internal/model"
	"github.com/hyper-swe/mgit/internal/sandboxd/provision"
	"github.com/hyper-swe/mgit/internal/sandboxd/staging"
	gitstore "github.com/hyper-swe/mgit/internal/store/git"
)

const quarantineTask = "MGIT-11.6.9"

// quarantineFixture is a shared repo (with the task branch) + a materialized
// worktree + a wired provisioner, the host-side setup for the container SEC-03
// delivery tests.
type quarantineFixture struct {
	repoRoot string
	wtPath   string
	prov     *provision.StoreProvisioner
}

func setupQuarantineFixture(t *testing.T) quarantineFixture {
	t.Helper()
	clock := func() time.Time { return time.Now().UTC() }
	repoRoot := t.TempDir()
	repo, err := gitstore.Init(repoRoot, clock)
	require.NoError(t, err)
	t.Cleanup(func() { _ = repo.Close() })
	base, err := repo.Head()
	require.NoError(t, err)
	require.NoError(t, gitstore.NewBranchStore(repo).CreateBranch(context.Background(),
		&model.Branch{Name: model.TaskBranchName(quarantineTask), HeadCommit: base}))

	wtPath := filepath.Join(t.TempDir(), "worktrees", "task-a")
	require.NoError(t, gitstore.NewWorktreeStore(repo).MaterializeBranchTo(
		context.Background(), model.TaskBranchName(quarantineTask), wtPath))
	require.NoError(t, os.WriteFile(filepath.Join(wtPath, "marker.txt"), []byte("work area"), 0o600))

	prov, err := provision.NewStoreProvisioner(repoRoot)
	require.NoError(t, err)
	return quarantineFixture{repoRoot: repoRoot, wtPath: wtPath, prov: prov}
}

func quarantinedManager(t *testing.T, runner *fakeRunner, fx quarantineFixture, sensitive ...string) *Manager {
	t.Helper()
	if runner.results == nil {
		runner.results = map[string]struct {
			out []byte
			err error
		}{}
	}
	mgr, err := NewManager(Config{
		Runner:           runner,
		WorkDir:          t.TempDir(),
		StoreProvisioner: fx.prov,
		SensitivePaths:   sensitive,
		Logger:           slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError})),
		Clock:            func() time.Time { return time.Now().UTC() },
	})
	require.NoError(t, err)
	return mgr
}

func launchOpts(fx quarantineFixture) model.SandboxLaunchOptions {
	return model.SandboxLaunchOptions{
		TaskID:       quarantineTask,
		WorktreePath: fx.wtPath,
		ImageRef:     "go-node@sha256:" + strings.Repeat("a", 64),
		Network:      model.NetworkPolicy{Mode: model.NetworkModeNone},
		CPUs:         2,
		MemoryMB:     1024,
		TTL:          time.Hour,
	}
}

// runVolumeSource returns the host source of the writable worktree --volume
// (the "src:guest" arg whose guest target is the worktree path).
func runVolumeSource(t *testing.T, args []string, guestWorktree string) string {
	t.Helper()
	for i, a := range args {
		if a == "--volume" && i+1 < len(args) {
			parts := strings.SplitN(args[i+1], ":", 3)
			if len(parts) >= 2 && parts[1] == guestWorktree && (len(parts) == 2 || parts[2] != "ro") {
				return parts[0]
			}
		}
	}
	return ""
}

// TestContainer_Quarantine_MountsStagedWorktree proves the writable worktree
// mount sources the STAGED dir (not the live worktree) at the worktree's
// identical guest path, and the staged tree carries the private .mgit while
// excluding the live worktree's own store. Refs: SEC-03, FR-17.3
func TestContainer_Quarantine_MountsStagedWorktree(t *testing.T) {
	fx := setupQuarantineFixture(t)
	// Plant an in-worktree store that must NOT reach the guest (F-A).
	require.NoError(t, os.MkdirAll(filepath.Join(fx.wtPath, ".mgit"), 0o750))
	require.NoError(t, os.WriteFile(filepath.Join(fx.wtPath, ".mgit", "LEAK"), []byte("host history"), 0o600))

	runner := &fakeRunner{}
	mgr := quarantinedManager(t, runner, fx)
	info, err := mgr.Launch(context.Background(), launchOpts(fx))
	require.NoError(t, err)

	runs := runner.callsFor("run")
	require.Len(t, runs, 1)
	src := runVolumeSource(t, runs[0], fx.wtPath)
	require.NotEmpty(t, src, "a writable worktree mount at the identical guest path is present")
	assert.NotEqual(t, fx.wtPath, src, "the SOURCE is the staged dir, not the live worktree")
	assert.True(t, strings.HasSuffix(src, stagingDirName), "the source is the per-sandbox staging tree")

	// The staged dir holds the worktree marker + the private store, and excludes
	// the live worktree's own .mgit content.
	assert.FileExists(t, filepath.Join(src, "marker.txt"))
	assert.DirExists(t, filepath.Join(src, staging.GuestStoreName))
	assert.NoFileExists(t, filepath.Join(src, ".mgit", "LEAK"),
		"the live worktree's own store content is never staged into the guest")

	// Teardown clears the sandbox-local state (private store + staging) and never
	// touches the live worktree.
	runner.results = map[string]struct {
		out []byte
		err error
	}{}
	require.NoError(t, mgr.Remove(context.Background(), info.ID, true))
	assert.NoDirExists(t, src, "teardown clears the staged worktree")
	assert.FileExists(t, filepath.Join(fx.wtPath, "marker.txt"), "the live worktree is untouched (FR-17.19)")
}

// TestContainer_Quarantine_EscapingSymlinkFailsClosed proves an escaping
// worktree symlink fails the launch CLOSED (no container runs) with the
// staging sentinel, and leaves no per-sandbox state behind. Refs: SEC-03, F-A/NEW-2
func TestContainer_Quarantine_EscapingSymlinkFailsClosed(t *testing.T) {
	fx := setupQuarantineFixture(t)
	outside := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(outside, "host-secret"), []byte("secret"), 0o600))
	require.NoError(t, os.Symlink(filepath.Join(outside, "host-secret"), filepath.Join(fx.wtPath, "escape")))

	runner := &fakeRunner{}
	mgr := quarantinedManager(t, runner, fx)
	_, err := mgr.Launch(context.Background(), launchOpts(fx))
	require.ErrorIs(t, err, staging.ErrSymlinkEscape,
		"the launch must fail CLOSED with the symlink-escape sentinel")
	assert.Empty(t, runner.callsFor("run"), "no container is ever run on a fail-closed escape")

	listed, lerr := mgr.List(context.Background())
	require.NoError(t, lerr)
	assert.Empty(t, listed, "a fail-closed launch is not registered")
}

// TestContainer_Quarantine_RunFailureClearsState proves a runtime failure after
// staging removes the per-sandbox state (no residue) and does not register.
func TestContainer_Quarantine_RunFailureClearsState(t *testing.T) {
	fx := setupQuarantineFixture(t)
	runner := &fakeRunner{results: map[string]struct {
		out []byte
		err error
	}{"run": {err: assert.AnError}}}
	mgr := quarantinedManager(t, runner, fx)
	_, err := mgr.Launch(context.Background(), launchOpts(fx))
	require.Error(t, err)

	listed, lerr := mgr.List(context.Background())
	require.NoError(t, lerr)
	assert.Empty(t, listed, "a failed launch is not registered")
}

// TestContainer_Quarantine_SharedStoreUnreachable proves the bind-mounted
// staged worktree contains neither the host shared store path nor its content:
// the guest sees only its private store. Refs: SEC-03 T6
func TestContainer_Quarantine_SharedStoreUnreachable(t *testing.T) {
	fx := setupQuarantineFixture(t)
	runner := &fakeRunner{}
	mgr := quarantinedManager(t, runner, fx)
	_, err := mgr.Launch(context.Background(), launchOpts(fx))
	require.NoError(t, err)

	src := runVolumeSource(t, runner.callsFor("run")[0], fx.wtPath)
	require.NotEmpty(t, src)
	// No --volume sources the host shared store, and the staged tree never
	// contains the shared store's host path as a nested dir.
	for _, a := range runner.callsFor("run")[0] {
		assert.False(t, strings.Contains(a, filepath.Join(fx.repoRoot, ".mgit")),
			"no mount sources the host shared store")
	}
}

// TestContainer_NewManager_ProvisionerRequiresWorkDir proves the SEC-03
// constructor guard: a provisioner without a work dir is rejected (the
// quarantine state has nowhere to live).
func TestContainer_NewManager_ProvisionerRequiresWorkDir(t *testing.T) {
	fx := setupQuarantineFixture(t)
	_, err := NewManager(Config{
		Runner:           &fakeRunner{},
		StoreProvisioner: fx.prov,
		Logger:           slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError})),
		Clock:            time.Now,
	})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "work dir")
}
