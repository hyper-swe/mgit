// Hostile-guest SEC-03 round-trip for the reduced-isolation container fallback
// against a real rootless podman container: the guest is the attacker. It
// proves the staged-mount quarantine holds — the host SHARED .mgit is not
// reachable inside the container, the in-worktree store content never reaches
// the guest, and the bind-mount sources the staged tree (not the live
// worktree). The container counterpart of firecracker's hostile_guest_linux_test.go.
//
// Gated on a usable rootless `podman` on PATH and a guest image carrying
// /bin/sh + test/grep/head (set MGIT_E2E_CONTAINER_IMAGE), so it skips rather
// than fails without them. Refs: SEC-03, FR-17.3, FR-17.15, MGIT-11.6.9
package container

import (
	"context"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/hyper-swe/mgit/internal/model"
	"github.com/hyper-swe/mgit/internal/sandboxd/provision"
	gitstore "github.com/hyper-swe/mgit/internal/store/git"
)

const hostileTask = "MGIT-11.6.9"

// requirePodman skips unless rootless podman is on PATH and a guest image is set.
func requirePodman(t *testing.T) (image string) {
	t.Helper()
	if _, err := exec.LookPath("podman"); err != nil {
		t.Skip("podman not on PATH; skipping the container hostile-guest e2e")
	}
	image = os.Getenv("MGIT_E2E_CONTAINER_IMAGE")
	if image == "" {
		t.Skip("set MGIT_E2E_CONTAINER_IMAGE to a pinned image (with /bin/sh + test/grep/head) to run the container hostile-guest e2e")
	}
	// A quick `podman info` confirms the rootless runtime actually works here.
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	if err := exec.CommandContext(ctx, "podman", "info").Run(); err != nil {
		t.Skipf("rootless podman not usable on this host: %v", err)
	}
	return image
}

// setupContainerHostileFixture builds a shared repo (task branch + base), an
// in-worktree store that must be excluded, the materialized worktree, and a
// wired provisioner.
func setupContainerHostileFixture(t *testing.T) (repoRoot, wtPath string, prov *provision.StoreProvisioner) {
	t.Helper()
	clock := func() time.Time { return time.Now().UTC() }
	repoRoot = t.TempDir()
	repo, err := gitstore.Init(repoRoot, clock)
	require.NoError(t, err)
	t.Cleanup(func() { _ = repo.Close() })
	base, err := repo.Head()
	require.NoError(t, err)
	require.NoError(t, gitstore.NewBranchStore(repo).CreateBranch(context.Background(),
		&model.Branch{Name: model.TaskBranchName(hostileTask), HeadCommit: base}))

	wtPath = filepath.Join(t.TempDir(), "worktrees", "task-a")
	require.NoError(t, gitstore.NewWorktreeStore(repo).MaterializeBranchTo(
		context.Background(), model.TaskBranchName(hostileTask), wtPath))
	require.NoError(t, os.WriteFile(filepath.Join(wtPath, "marker.txt"), []byte("work area"), 0o600))
	// An in-worktree store that must NOT reach the guest (F-A).
	require.NoError(t, os.MkdirAll(filepath.Join(wtPath, ".mgit"), 0o750))
	require.NoError(t, os.WriteFile(filepath.Join(wtPath, ".mgit", "LEAK"), []byte("clone history"), 0o600))

	prov, err = provision.NewStoreProvisioner(repoRoot)
	require.NoError(t, err)
	return repoRoot, wtPath, prov
}

// TestE2E_Container_HostileGuest_SharedStoreUnreachable boots a real podman
// container and proves the host shared store is not reachable and the in-
// worktree store content never reaches the guest, while the private store is
// present. Refs: SEC-03 T6
func TestE2E_Container_HostileGuest_SharedStoreUnreachable(t *testing.T) {
	image := requirePodman(t)
	repoRoot, wtPath, prov := setupContainerHostileFixture(t)

	mgr, err := NewManager(Config{
		Runner:           PodmanRunner{},
		WorkDir:          t.TempDir(),
		StoreProvisioner: prov,
		SensitivePaths:   model.DefaultSandboxPolicy().SensitivePaths,
		Logger:           slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError})),
		Clock:            func() time.Time { return time.Now().UTC() },
	})
	require.NoError(t, err)

	info, err := mgr.Launch(context.Background(), model.SandboxLaunchOptions{
		TaskID: hostileTask, WorktreePath: wtPath, ImageRef: image,
		Network: model.NetworkPolicy{Mode: model.NetworkModeNone}, CPUs: 1, MemoryMB: 256,
	})
	require.NoError(t, err)
	t.Cleanup(func() { _ = mgr.Remove(context.Background(), info.ID, true) })

	// Inside the container: the private .mgit is present; the host shared store
	// path is NOT; the in-worktree store's LEAK content is gone. The probe is
	// scoped to the worktree subtree (never `/`).
	res, err := mgr.Exec(context.Background(), info.ID, model.ExecRequest{
		Command: []string{"/bin/sh", "-c",
			"test -d " + filepath.Join(wtPath, ".mgit") + " && echo PRIVATE_OK; " +
				"test -e " + filepath.Join(repoRoot, ".mgit") + " && echo SHARED_LEAK || echo SHARED_ABSENT; " +
				"grep -rl 'work area' " + wtPath + " 2>/dev/null | head -n1; echo ---; " +
				"grep -rl 'clone history' " + wtPath + " 2>/dev/null | head -n1 || true"},
	})
	require.NoError(t, err)
	out := string(res.Stdout)
	assert.Contains(t, out, "PRIVATE_OK", "the guest's private .mgit store is present")
	assert.Contains(t, out, "SHARED_ABSENT", "the host shared store is not reachable from the container")
	assert.NotContains(t, out, "SHARED_LEAK")

	parts := strings.SplitN(out, "---", 2)
	require.Len(t, parts, 2, "probe produced both halves")
	assert.Contains(t, parts[0], "marker.txt",
		"positive control: grep finds the worktree marker (proves grep works)")
	assert.NotContains(t, parts[1], "clone history",
		"the in-worktree store content is never reachable from the guest (F-A)")
}
