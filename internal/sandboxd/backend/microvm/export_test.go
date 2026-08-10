package microvm

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/hyper-swe/mgit/internal/model"
	"github.com/hyper-swe/mgit/internal/sandboxd/artifactexport"
)

// launchWithStagedTree boots a fake-hypervisor sandbox and plants a staged
// worktree directory for it — the host directory that, on the virtiofs
// backends, IS the guest's worktree. Returns the sandbox ID and the staged
// tree path so a test can write "what the guest built" into it.
func launchWithStagedTree(t *testing.T, mgr *Manager, workDir, task string) (sandboxID, staged string) {
	t.Helper()
	info, err := mgr.Launch(context.Background(), launchOpts(task, model.NetworkModeNone))
	require.NoError(t, err)
	staged = filepath.Join(SandboxStateDir(workDir, info.ID), stagedTreeName)
	require.NoError(t, os.MkdirAll(staged, 0o750))
	return info.ID, staged
}

// guestBuilds writes a file into the staged tree, standing in for work the
// guest did inside its sandbox.
func guestBuilds(t *testing.T, staged, rel, content string) {
	t.Helper()
	path := filepath.Join(staged, rel)
	require.NoError(t, os.MkdirAll(filepath.Dir(path), 0o750))
	require.NoError(t, os.WriteFile(path, []byte(content), 0o600))
}

func TestManager_ExportArtifact_GuestBuiltTree_LandsOnTheHost(t *testing.T) {
	mgr, workDir := testManager(t, &fakeHypervisor{})
	id, staged := launchWithStagedTree(t, mgr, workDir, "MGIT-73")
	guestBuilds(t, staged, "node_modules/pkg/index.js", "module.exports = 1\n")

	dest := filepath.Join(t.TempDir(), "cache-node-modules")
	res, err := mgr.ExportArtifact(context.Background(), id,
		model.ArtifactExportRequest{GuestPath: "node_modules", HostPath: dest})

	require.NoError(t, err)
	assert.Equal(t, id, res.SandboxID)
	assert.Equal(t, "MGIT-73", res.TaskID)
	assert.Equal(t, dest, res.HostPath)
	assert.Equal(t, 1, res.Files)
	assert.NotEmpty(t, res.TreeHash)

	got, err := os.ReadFile(filepath.Join(dest, "pkg", "index.js")) //nolint:gosec // test-owned temp dir
	require.NoError(t, err)
	assert.Equal(t, "module.exports = 1\n", string(got))

	assert.FileExists(t, dest+artifactexport.ManifestSuffix,
		"an exported artifact always lands with its provenance sidecar")
}

func TestManager_ExportArtifact_EscapingSymlink_RefusedWithNothingWritten(t *testing.T) {
	mgr, workDir := testManager(t, &fakeHypervisor{})
	id, staged := launchWithStagedTree(t, mgr, workDir, "MGIT-73")
	guestBuilds(t, staged, "out/real.txt", "fine\n")
	secret := filepath.Join(t.TempDir(), "host-secret")
	require.NoError(t, os.WriteFile(secret, []byte("host only\n"), 0o600))
	require.NoError(t, os.Symlink(secret, filepath.Join(staged, "out", "escape")))

	hostDir := t.TempDir()
	_, err := mgr.ExportArtifact(context.Background(), id,
		model.ArtifactExportRequest{GuestPath: "out", HostPath: filepath.Join(hostDir, "artifact")})

	require.Error(t, err)
	entries, readErr := os.ReadDir(hostDir)
	require.NoError(t, readErr)
	assert.Empty(t, entries, "a refused export writes nothing to the host")
}

func TestManager_ExportArtifact_NoStagedTree_ReportsTheBackendLimitation(t *testing.T) {
	mgr, _ := testManager(t, &fakeHypervisor{})
	info, err := mgr.Launch(context.Background(), launchOpts("MGIT-73", model.NetworkModeNone))
	require.NoError(t, err)

	_, err = mgr.ExportArtifact(context.Background(), info.ID,
		model.ArtifactExportRequest{GuestPath: "out", HostPath: filepath.Join(t.TempDir(), "artifact")})

	require.ErrorIs(t, err, model.ErrArtifactExportUnsupported,
		"a launch-time-image backend must fail CLOSED, naming the limitation")
}

func TestManager_ExportArtifact_UnknownSandbox_ReturnsNotFound(t *testing.T) {
	mgr, _ := testManager(t, &fakeHypervisor{})

	_, err := mgr.ExportArtifact(context.Background(), "sbx-missing",
		model.ArtifactExportRequest{GuestPath: "out", HostPath: filepath.Join(t.TempDir(), "artifact")})

	require.ErrorIs(t, err, model.ErrSandboxNotFound)
}

func TestManager_ExportArtifact_SuspendedSandbox_IsRefused(t *testing.T) {
	mgr, workDir := testManager(t, &fakeHypervisor{})
	id, staged := launchWithStagedTree(t, mgr, workDir, "MGIT-73")
	guestBuilds(t, staged, "out/real.txt", "fine\n")
	require.NoError(t, mgr.Stop(context.Background(), id, false)) // records the sandbox suspended

	_, err := mgr.ExportArtifact(context.Background(), id,
		model.ArtifactExportRequest{GuestPath: "out", HostPath: filepath.Join(t.TempDir(), "artifact")})

	require.ErrorIs(t, err, model.ErrSandboxBackendUnavailable)
}

func TestManager_ExportArtifact_InvalidRequest_IsRejectedAtTheBoundary(t *testing.T) {
	mgr, workDir := testManager(t, &fakeHypervisor{})
	id, staged := launchWithStagedTree(t, mgr, workDir, "MGIT-73")
	guestBuilds(t, staged, "out/real.txt", "fine\n")

	tests := []struct {
		name string
		req  model.ArtifactExportRequest
	}{
		{name: "absolute_guest_path", req: model.ArtifactExportRequest{GuestPath: "/etc", HostPath: "/tmp/x"}},
		{name: "empty_guest_path", req: model.ArtifactExportRequest{GuestPath: "", HostPath: "/tmp/x"}},
		{name: "relative_host_path", req: model.ArtifactExportRequest{GuestPath: "out", HostPath: "relative"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := mgr.ExportArtifact(context.Background(), id, tt.req)
			require.Error(t, err)
		})
	}
}

// TestManager_ExportArtifact_HostileGuest_CannotWriteOutsideTheNamedDestination
// is the export counterpart of the LandIsOnlyBridge invariant, restated for the
// new bridge: a hostile guest that has filled its worktree with escape attempts
// still cannot cause a single host write outside the destination the HOST
// named — and it cannot get its private store out at all, so committed objects
// still cross only through the verified land path.
// Refs: MGIT-73, SEC-03, SEC-10, ADR-011
func TestManager_ExportArtifact_HostileGuest_CannotWriteOutsideTheNamedDestination(t *testing.T) {
	mgr, workDir := testManager(t, &fakeHypervisor{})
	id, staged := launchWithStagedTree(t, mgr, workDir, "MGIT-73")

	// What a hostile guest leaves behind, all inside its own worktree.
	guestBuilds(t, staged, "out/legit.txt", "ordinary build output\n")
	guestBuilds(t, staged, ".mgit/HEAD", "ref: refs/heads/task/MGIT-73\n")
	guestBuilds(t, staged, "sibling/secret-in-worktree.txt", "not part of out/\n")

	offLimits := t.TempDir()
	hostSecret := filepath.Join(offLimits, "host-secret")
	require.NoError(t, os.WriteFile(hostSecret, []byte("host only\n"), 0o600))
	require.NoError(t, os.Symlink(hostSecret, filepath.Join(staged, "out", "abs-link")))
	require.NoError(t, os.Symlink("../sibling/secret-in-worktree.txt", filepath.Join(staged, "out", "rel-escape")))

	hostDir := t.TempDir()
	attempts := []struct {
		name      string
		guestPath string
		hostPath  string
	}{
		{name: "subtree_with_escaping_links", guestPath: "out", hostPath: filepath.Join(hostDir, "a")},
		{name: "the_private_store", guestPath: ".mgit", hostPath: filepath.Join(hostDir, "b")},
		{name: "traversal_out_of_the_worktree", guestPath: "../../etc", hostPath: filepath.Join(hostDir, "c")},
		{name: "absolute_guest_path", guestPath: "/etc/passwd", hostPath: filepath.Join(hostDir, "d")},
		{name: "whole_worktree", guestPath: ".", hostPath: filepath.Join(hostDir, "e")},
	}
	for _, tt := range attempts {
		t.Run(tt.name, func(t *testing.T) {
			_, err := mgr.ExportArtifact(context.Background(), id,
				model.ArtifactExportRequest{GuestPath: tt.guestPath, HostPath: tt.hostPath})
			require.Error(t, err, "the export must fail closed")
		})
	}

	entries, err := os.ReadDir(hostDir)
	require.NoError(t, err)
	assert.Empty(t, entries, "no refused export wrote anything to the host")
	assert.FileExists(t, hostSecret, "the host file a guest symlink pointed at is untouched")
	content, err := os.ReadFile(hostSecret) //nolint:gosec // test-owned temp dir
	require.NoError(t, err)
	assert.Equal(t, "host only\n", string(content))

	// And the honest positive half: with the escape attempts removed, the same
	// verb DOES deliver the legitimate artifact — a deny-only proof would not
	// distinguish containment from a broken feature.
	require.NoError(t, os.Remove(filepath.Join(staged, "out", "abs-link")))
	require.NoError(t, os.Remove(filepath.Join(staged, "out", "rel-escape")))
	res, err := mgr.ExportArtifact(context.Background(), id,
		model.ArtifactExportRequest{GuestPath: "out", HostPath: filepath.Join(hostDir, "ok")})
	require.NoError(t, err)
	assert.Equal(t, 1, res.Files)
	got, err := os.ReadFile(filepath.Join(hostDir, "ok", "legit.txt")) //nolint:gosec // test-owned temp dir
	require.NoError(t, err)
	assert.Equal(t, "ordinary build output\n", string(got))
}
