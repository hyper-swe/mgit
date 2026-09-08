//go:build linux

package firecracker

import (
	"context"
	"fmt"
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
	"github.com/hyper-swe/mgit/internal/sandboxd/images"
)

// TestE2E_Exec_RunsAsTheIdentityAsked is the live proof of MGIT-151 on
// firecracker: a real guest, the worktree delivered as an ext4 image whose
// inodes carry the host's uid/gid (mke2fs -d), and a command asked to run
// as that identity reports it from its OWN view — uid, gid, the name the
// guest's passwd resolves, HOME, and the owner of a file it writes — and
// then writes the worktree as it. In the unprivileged CI half the host is
// the runner user, so this is the real switch and the real write; in the
// root half the host is root and the identity is root, and the guest still
// says so. Asked for nothing, the guest reports root. Refs: MGIT-151, FR-17.11
func TestE2E_Exec_RunsAsTheIdentityAsked(t *testing.T) {
	kernel, _ := requireKVM(t)
	rootfs := os.Getenv("MGIT_E2E_GUEST_ROOTFS")
	if rootfs == "" || !fileExists(rootfs) {
		t.Skip("set MGIT_E2E_GUEST_ROOTFS to a present guest image")
	}
	wtPath := filepath.Join(t.TempDir(), "repo", "worktrees", "task-id")
	require.NoError(t, os.MkdirAll(wtPath, 0o750))
	// The probe rides INSIDE the worktree image, the one place a runtime
	// binary can reach a firecracker guest; it is exec'd at its identical
	// guest path.
	probe := filepath.Join(wtPath, "idprobe")
	build := exec.Command("go", "build", "-trimpath", "-buildvcs=false", "-ldflags=-buildid=", //nolint:gosec // G204: fixed argv, temp output
		"-o", probe, "./internal/sandboxd/backend/libkrun/testdata/idprobe")
	build.Dir = repoRootDir(t)
	build.Env = append(os.Environ(), "GOOS=linux", "GOARCH=amd64", "CGO_ENABLED=0")
	out, err := build.CombinedOutput()
	require.NoError(t, err, "build idprobe: %s", out)

	clock := func() time.Time { return time.Now().UTC() }
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
	workDir, err := os.MkdirTemp("", "mgid")
	require.NoError(t, err)
	t.Cleanup(func() { _ = os.RemoveAll(workDir) })
	mgr, err := NewManager(Config{
		WorkDir: workDir,
		Resolve: func(r string) (ImagePaths, error) {
			ri, rerr := store.Resolve(r)
			return ImagePaths{KernelPath: ri.KernelPath, RootfsPath: ri.RootfsPath, Cmdline: ri.Cmdline}, rerr
		},
		Logger: slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError})),
		Clock:  clock,
	})
	require.NoError(t, err)
	info, err := mgr.Launch(context.Background(), model.SandboxLaunchOptions{
		TaskID: "MGIT-151", WorktreePath: wtPath, ImageRef: ref,
		Network: model.NetworkPolicy{Mode: model.NetworkModeNone}, CPUs: 1, MemoryMB: 256,
	})
	require.NoError(t, err)
	t.Cleanup(func() { _ = mgr.Remove(context.Background(), info.ID, true) })

	asked := model.GuestIdentity{UID: os.Getuid(), GID: os.Getgid(), Name: "agent", Home: "/home/agent"}
	res := execWhenServing(t, mgr, info.ID, model.ExecRequest{
		Command: []string{probe}, Dir: wtPath, RunAs: &asked,
	})
	want := fmt.Sprintf("uid=%d gid=%d name=agent home=/home/agent home_file_owner=%d:%d", asked.UID, asked.GID, asked.UID, asked.GID)
	assert.Equal(t, want, strings.TrimSpace(string(res.Stdout)), "the command's own view of its identity; stderr=%q", string(res.Stderr))
	require.NotNil(t, res.RanAs, "the guest echoes the identity it ran as")
	assert.Equal(t, asked, *res.RanAs)

	// The worktree — delivered as an image owned by the host's uid — is
	// writable as that identity, and what it writes is owned by it.
	wt := execWhenServing(t, mgr, info.ID, model.ExecRequest{
		Command: []string{"/bin/sh", "-c", "touch owned && stat -c %u:%g owned"}, Dir: wtPath, RunAs: &asked,
	})
	assert.Equal(t, 0, wt.ExitCode, "the identity writes the worktree; stderr=%q", string(wt.Stderr))
	assert.Equal(t, fmt.Sprintf("%d:%d", asked.UID, asked.GID), strings.TrimSpace(string(wt.Stdout)), "and owns what it wrote")

	// Nothing asked: the guest runs it as itself, root, and SAYS so.
	none := execWhenServing(t, mgr, info.ID, model.ExecRequest{Command: []string{probe}, Dir: wtPath})
	assert.True(t, strings.HasPrefix(string(none.Stdout), "uid=0 gid=0 "), "nothing asked runs as the guest's own root: %q", string(none.Stdout))
	require.NotNil(t, none.RanAs)
	assert.True(t, none.RanAs.IsRoot(), "and the guest reports root")
	if !t.Failed() {
		t.Logf("IDENTITY REAL VM PASS: asked uid %d gid %d; the guest's child saw %q; the worktree write is owned %s; nothing asked ran as root and said so",
			asked.UID, asked.GID, strings.TrimSpace(string(res.Stdout)), strings.TrimSpace(string(wt.Stdout)))
	}
}

// execWhenServing retries an exec until the guest serves vsock (boot is
// async), then returns its result.
func execWhenServing(t *testing.T, mgr interface {
	Exec(context.Context, string, model.ExecRequest) (*model.ExecResult, error)
}, id string, req model.ExecRequest) *model.ExecResult {
	t.Helper()
	var (
		res *model.ExecResult
		err error
	)
	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) {
		res, err = mgr.Exec(context.Background(), id, req)
		if err == nil {
			return res
		}
		time.Sleep(400 * time.Millisecond)
	}
	require.NoError(t, err, "exec must reach the guest once it serves vsock")
	return res
}

// repoRootDir walks up from the package to the module root (go.mod).
func repoRootDir(t *testing.T) string {
	t.Helper()
	dir, err := os.Getwd()
	require.NoError(t, err)
	for {
		if fileExists(filepath.Join(dir, "go.mod")) {
			return dir
		}
		parent := filepath.Dir(dir)
		require.NotEqual(t, dir, parent, "go.mod not found above %s", dir)
		dir = parent
	}
}
