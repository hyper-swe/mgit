//go:build darwin && cgo

// Mode fidelity across the vzf virtio-fs share, and through an export
// (MGIT-81). libkrun's macOS filesystem device presents guest-created inodes
// with placeholder permission bits and keeps the real st_mode in a share
// record; the export learned to read that record, and this is the other half
// of the same question: does Virtualization.framework's virtio-fs do the same?
//
// It is a measurement, not a restatement — the mapping lives inside the
// framework and no host-side reasoning settles it. A static probe binary is
// dropped into the shared worktree host-side and EXECUTED INSIDE the guest
// (the vzf guest image is minimal: no chmod, no stat, no coreutils), it
// reports the modes the guest sees, and the host then compares its own view
// and what an export produces. Refs: MGIT-81, MGIT-73, ADR-011
package vzf

import (
	"context"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/hyper-swe/mgit/internal/model"
	"github.com/hyper-swe/mgit/internal/sandboxd/backend/microvm"
	"github.com/hyper-swe/mgit/internal/sandboxd/provision"
	gitstore "github.com/hyper-swe/mgit/internal/store/git"
)

// modeProbeTask is the task the mode-fidelity sandbox is bound to.
const modeProbeTask = "MGIT-81"

// modeProbeCases are the probe's case names in report order, paired with the
// mode each one ends up asking the guest kernel for (umask 0022 applies to the
// create modes; a chmod is verbatim).
var modeProbeCases = []struct {
	name string
	want fs.FileMode
}{
	{"create-0755", 0o755},
	{"create-0644", 0o644},
	{"create-0600", 0o600},
	{"chmod-0755", 0o755},
	{"dir-0755", 0o755},
}

// buildModeProbe cross-compiles the guest probe into the worktree, from where
// the share carries it into the guest at the identical path.
func buildModeProbe(t *testing.T, worktree string) string {
	t.Helper()
	_, thisFile, _, ok := runtime.Caller(0)
	require.True(t, ok, "locate the test source to find testdata/modeprobe")
	out := filepath.Join(worktree, "modeprobe")
	cmd := exec.Command("go", "build", "-o", out, ".") //nolint:gosec // fixed argv
	cmd.Dir = filepath.Join(filepath.Dir(thisFile), "testdata", "modeprobe")
	cmd.Env = append(os.Environ(), "GOOS=linux", "GOARCH="+runtime.GOARCH, "CGO_ENABLED=0")
	combined, err := cmd.CombinedOutput()
	require.NoError(t, err, "build the guest mode probe:\n%s", combined)
	return out
}

// modeProbeWorktree materializes a worktree for the sandbox, separate from the
// shared repo (a worktree containing the shared store is refused at launch by
// the SEC-03 quarantine check).
func modeProbeWorktree(t *testing.T) (repoRoot, worktree string) {
	t.Helper()
	clock := func() time.Time { return time.Now().UTC() }
	repoRoot = t.TempDir()
	repo, err := gitstore.Init(repoRoot, clock)
	require.NoError(t, err)
	t.Cleanup(func() { _ = repo.Close() })
	base, err := repo.Head()
	require.NoError(t, err)
	require.NoError(t, gitstore.NewBranchStore(repo).CreateBranch(context.Background(),
		&model.Branch{Name: model.TaskBranchName(modeProbeTask), HeadCommit: base}))
	worktree = filepath.Join(t.TempDir(), "wt")
	require.NoError(t, gitstore.NewWorktreeStore(repo).MaterializeBranchTo(
		context.Background(), model.TaskBranchName(modeProbeTask), worktree))
	return repoRoot, worktree
}

// runModeProbeInGuest execs the probe inside the booted guest, retrying while
// the guest is still coming up, and returns the modes it reported.
func runModeProbeInGuest(t *testing.T, mgr *microvm.Manager, id, probe, target string) map[string]fs.FileMode {
	t.Helper()
	var res *model.ExecResult
	var err error
	deadline := time.Now().Add(25 * time.Second)
	for time.Now().Before(deadline) {
		res, err = mgr.Exec(context.Background(), id,
			model.ExecRequest{Command: []string{probe, target}})
		if err == nil {
			break
		}
		time.Sleep(300 * time.Millisecond)
	}
	require.NoError(t, err, "the guest never ran the mode probe")
	require.Equal(t, 0, res.ExitCode, "probe stdout:\n%s\nstderr:\n%s", res.Stdout, res.Stderr)
	t.Logf("guest probe:\n%s", res.Stdout)

	got := map[string]fs.FileMode{}
	for _, line := range strings.Split(string(res.Stdout), "\n") {
		fields := strings.Fields(line)
		if len(fields) != 4 || fields[0] != "MODEPROBE" {
			continue
		}
		var mode uint32
		if _, serr := fmt.Sscanf(fields[3], "got=%o", &mode); serr != nil {
			continue
		}
		got[fields[1]] = fs.FileMode(mode)
	}
	return got
}

// TestE2E_VZF_ModeFidelity_TheShareCarriesTheModeAndExportReproducesIt is the
// vzf arm of the MGIT-81 measurement: unlike libkrun's macOS device,
// Virtualization.framework's virtio-fs carries modes in the host file's own
// permission bits, so the host observes them by a plain stat and an export
// needs no share record. It fails if that ever changes — at which point the
// export's fallback would be silently producing the wrong modes here.
// Refs: MGIT-81
func TestE2E_VZF_ModeFidelity_TheShareCarriesTheModeAndExportReproducesIt(t *testing.T) {
	kernel, rootfs := requireE2EGuest(t)
	repoRoot, worktree := modeProbeWorktree(t)
	probe := buildModeProbe(t, worktree)
	prov, err := provision.NewStoreProvisioner(repoRoot)
	require.NoError(t, err)

	workDir := t.TempDir()
	mgr, err := NewManager(Config{
		WorkDir: workDir,
		Resolve: func(string) (ImagePaths, error) {
			return ImagePaths{KernelPath: kernel, RootfsPath: rootfs, Cmdline: e2eVZFCmdline}, nil
		},
		StoreProvisioner: prov,
		Logger:           slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError})),
		Clock:            func() time.Time { return time.Now().UTC() },
	})
	require.NoError(t, err)
	info, err := mgr.Launch(context.Background(), model.SandboxLaunchOptions{
		TaskID: modeProbeTask, WorktreePath: worktree,
		ImageRef: "mgit-guest@sha256:" + strings.Repeat("a", 64),
		Network:  model.NetworkPolicy{Mode: model.NetworkModeNone}, CPUs: 1, MemoryMB: 512,
	})
	if err != nil && strings.Contains(err.Error(), "com.apple.security.virtualization") {
		t.Skipf("test binary lacks the virtualization entitlement: %v", err)
	}
	require.NoError(t, err)
	t.Cleanup(func() { _ = mgr.Remove(context.Background(), info.ID, true) })

	guest := runModeProbeInGuest(t, mgr, info.ID, probe, filepath.Join(worktree, "mp"))
	staged := filepath.Join(microvm.SandboxStateDir(workDir, info.ID), stagingDirName)

	dest := filepath.Join(t.TempDir(), "mp")
	res, err := mgr.ExportArtifact(context.Background(), info.ID,
		model.ArtifactExportRequest{GuestPath: "mp", HostPath: dest})
	require.NoError(t, err, "export the guest-built probe tree")
	t.Logf("exported %d entries (tree %s)", res.Files, res.TreeHash)

	t.Log("case          guest wanted  guest saw  host saw   exported")
	for _, c := range modeProbeCases {
		require.Contains(t, guest, c.name, "the probe reported this case")
		assert.Equal(t, c.want, guest[c.name], "%s: the guest kept the mode it set", c.name)

		host, err := os.Lstat(filepath.Join(staged, "mp", c.name))
		require.NoError(t, err)
		exported, xerr := os.Lstat(filepath.Join(dest, c.name))
		require.NoError(t, xerr)
		t.Logf("%-13s %04o          %04o       %04o       %04o",
			c.name, c.want, guest[c.name], host.Mode().Perm(), exported.Mode().Perm())

		assert.Equal(t, guest[c.name], host.Mode().Perm(),
			"%s: vzf's share must carry the mode in the host file's own bits", c.name)
		if !host.IsDir() {
			assert.Equal(t, guest[c.name], exported.Mode().Perm(),
				"%s: the export must reproduce the mode the guest set", c.name)
		}
	}
}
