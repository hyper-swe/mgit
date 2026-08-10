package artifactexport

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/hyper-swe/mgit/internal/sandboxd/staging"
)

// fixedNow is the injected clock reading every test exports at (no time.Now
// anywhere in this package).
var fixedNow = time.Date(2026, 8, 10, 12, 0, 0, 0, time.UTC)

// stagedFixture builds a staged tree that looks like what a guest leaves
// behind: a build output directory with nested files, an in-subtree symlink,
// and the private store beside it.
func stagedFixture(t *testing.T) string {
	t.Helper()
	staged := t.TempDir()
	mustWrite(t, filepath.Join(staged, "src", "main.go"), "package main\n", 0o644)
	mustWrite(t, filepath.Join(staged, staging.GuestStoreName, "HEAD"), "ref: refs/heads/task/MGIT-73\n", 0o600)
	mustWrite(t, filepath.Join(staged, "node_modules", "pkg", "index.js"), "module.exports = 1\n", 0o644)
	mustWrite(t, filepath.Join(staged, "node_modules", "pkg", "bin", "run.sh"), "#!/bin/sh\n", 0o755)
	mustWrite(t, filepath.Join(staged, "node_modules", ".bin", "target.js"), "// target\n", 0o644)
	require.NoError(t, os.Symlink("../pkg/index.js", filepath.Join(staged, "node_modules", ".bin", "pkg")))
	return staged
}

// mustWrite writes a file and its parents.
func mustWrite(t *testing.T, path, content string, mode os.FileMode) {
	t.Helper()
	require.NoError(t, os.MkdirAll(filepath.Dir(path), 0o750))
	require.NoError(t, os.WriteFile(path, []byte(content), mode))
}

// destIn returns a not-yet-existing destination inside a fresh host dir,
// together with that dir (so a test can prove NOTHING was written).
func destIn(t *testing.T) (hostDir, dest string) {
	t.Helper()
	hostDir = t.TempDir()
	return hostDir, filepath.Join(hostDir, "artifact")
}

// request builds a valid export request for a staged tree.
func request(staged, guestPath, dest string) Request {
	return Request{
		StagedTree: staged,
		GuestPath:  guestPath,
		HostPath:   dest,
		Now:        fixedNow,
		Provenance: Provenance{
			SandboxID: "sbx-1", TaskID: "MGIT-73", Backend: "libkrun",
			BaseDigest: "sha256:" + strings.Repeat("a", 64),
		},
	}
}

// hostDirEntries lists the names directly under a host directory, so a test
// can assert an export wrote NOTHING (not even a temp directory).
func hostDirEntries(t *testing.T, dir string) []string {
	t.Helper()
	entries, err := os.ReadDir(dir)
	require.NoError(t, err)
	names := make([]string, 0, len(entries))
	for _, e := range entries {
		names = append(names, e.Name())
	}
	return names
}

func TestExport_GuestBuiltTree_LandsOnTheHostIntact(t *testing.T) {
	staged := stagedFixture(t)
	_, dest := destIn(t)

	res, err := Export(request(staged, "node_modules", dest))
	require.NoError(t, err)

	assert.Equal(t, dest, res.HostPath)
	assert.Equal(t, 4, res.Files, "3 regular files + 1 symlink")
	assert.NotEmpty(t, res.TreeHash)

	got, err := os.ReadFile(filepath.Join(dest, "pkg", "index.js")) //nolint:gosec // test-owned temp dir
	require.NoError(t, err)
	assert.Equal(t, "module.exports = 1\n", string(got))

	fi, err := os.Lstat(filepath.Join(dest, "pkg", "bin", "run.sh"))
	require.NoError(t, err)
	assert.Equal(t, os.FileMode(0o755), fi.Mode().Perm(), "the executable bit must survive the export")

	link, err := os.Readlink(filepath.Join(dest, ".bin", "pkg"))
	require.NoError(t, err)
	assert.Equal(t, "../pkg/index.js", link, "an in-subtree symlink is exported as a symlink")
}

func TestExport_SingleFile_LandsOnTheHost(t *testing.T) {
	staged := stagedFixture(t)
	_, dest := destIn(t)

	res, err := Export(request(staged, "src/main.go", dest))
	require.NoError(t, err)
	assert.Equal(t, 1, res.Files)

	got, err := os.ReadFile(dest) //nolint:gosec // test-owned temp dir
	require.NoError(t, err)
	assert.Equal(t, "package main\n", string(got))
}

func TestExport_EscapingSymlink_RefusedBeforeAnyHostWrite(t *testing.T) {
	staged := stagedFixture(t)
	outside := filepath.Join(t.TempDir(), "secret.txt")
	mustWrite(t, outside, "host secret\n", 0o600)
	require.NoError(t, os.Symlink(outside, filepath.Join(staged, "node_modules", "escape")))

	hostDir, dest := destIn(t)
	res, err := Export(request(staged, "node_modules", dest))

	require.ErrorIs(t, err, staging.ErrSymlinkEscape,
		"an escaping symlink must fail CLOSED with the staging symlink-escape error")
	assert.Nil(t, res)
	assert.Empty(t, hostDirEntries(t, hostDir),
		"nothing — not even a partial or temporary tree — may be written before the refusal")
}

func TestExport_SymlinkLeavingTheExportedSubtree_Refused(t *testing.T) {
	staged := stagedFixture(t)
	// Inside the staged tree, but OUTSIDE the exported subtree: after the
	// export the link would dangle or resolve to an unrelated host path.
	require.NoError(t, os.Symlink("../src/main.go", filepath.Join(staged, "node_modules", "up")))

	hostDir, dest := destIn(t)
	_, err := Export(request(staged, "node_modules", dest))

	require.ErrorIs(t, err, staging.ErrSymlinkEscape)
	assert.Empty(t, hostDirEntries(t, hostDir))
}

func TestExport_SourceIsASymlink_Refused(t *testing.T) {
	staged := stagedFixture(t)
	require.NoError(t, os.Symlink("node_modules", filepath.Join(staged, "alias")))

	hostDir, dest := destIn(t)
	_, err := Export(request(staged, "alias", dest))

	require.ErrorIs(t, err, ErrUnsafePath)
	assert.Empty(t, hostDirEntries(t, hostDir))
}

func TestExport_UnsafeGuestPath_Refused(t *testing.T) {
	tests := []struct {
		name      string
		guestPath string
	}{
		{name: "empty", guestPath: ""},
		{name: "absolute", guestPath: "/etc/passwd"},
		{name: "parent_traversal", guestPath: ".."},
		{name: "embedded_traversal", guestPath: "node_modules/../../etc"},
		{name: "leading_traversal", guestPath: "../outside"},
		{name: "whole_worktree", guestPath: "."},
		{name: "nul_byte", guestPath: "node_modules\x00/pkg"},
		{name: "private_store", guestPath: staging.GuestStoreName},
		{name: "inside_private_store", guestPath: staging.GuestStoreName + "/HEAD"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			staged := stagedFixture(t)
			hostDir, dest := destIn(t)

			_, err := Export(request(staged, tt.guestPath, dest))

			require.ErrorIs(t, err, ErrUnsafePath)
			assert.Empty(t, hostDirEntries(t, hostDir))
		})
	}
}

func TestExport_MissingGuestPath_Refused(t *testing.T) {
	staged := stagedFixture(t)
	hostDir, dest := destIn(t)

	_, err := Export(request(staged, "no/such/dir", dest))

	require.ErrorIs(t, err, ErrSourceNotFound)
	assert.Empty(t, hostDirEntries(t, hostDir))
}

func TestExport_CollidingHostPath_RefusedAndLeavesTheHostFileUntouched(t *testing.T) {
	tests := []struct {
		name    string
		prepare func(t *testing.T, dest string)
	}{
		{
			name: "existing_file",
			prepare: func(t *testing.T, dest string) {
				mustWrite(t, dest, "precious host content\n", 0o600)
			},
		},
		{
			name: "existing_directory",
			prepare: func(t *testing.T, dest string) {
				require.NoError(t, os.MkdirAll(filepath.Join(dest, "keep"), 0o750))
			},
		},
		{
			name: "existing_manifest",
			prepare: func(t *testing.T, dest string) {
				mustWrite(t, dest+ManifestSuffix, "{}\n", 0o600)
			},
		},
		{
			name: "existing_dangling_symlink",
			prepare: func(t *testing.T, dest string) {
				require.NoError(t, os.Symlink("/nowhere", dest))
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			staged := stagedFixture(t)
			hostDir, dest := destIn(t)
			tt.prepare(t, dest)
			before := hostDirEntries(t, hostDir)

			_, err := Export(request(staged, "node_modules", dest))

			require.ErrorIs(t, err, ErrCollision, "refusing is the documented collision policy")
			assert.ElementsMatch(t, before, hostDirEntries(t, hostDir),
				"a refused export must not add, replace, or remove anything on the host")
		})
	}
}

func TestExport_CollidingHostPath_PreservesTheExistingContent(t *testing.T) {
	staged := stagedFixture(t)
	_, dest := destIn(t)
	mustWrite(t, dest, "precious host content\n", 0o600)

	_, err := Export(request(staged, "node_modules", dest))
	require.ErrorIs(t, err, ErrCollision)

	got, err := os.ReadFile(dest) //nolint:gosec // test-owned temp dir
	require.NoError(t, err)
	assert.Equal(t, "precious host content\n", string(got))
}

func TestExport_MissingHostParentDirectory_Refused(t *testing.T) {
	staged := stagedFixture(t)
	hostDir, _ := destIn(t)
	dest := filepath.Join(hostDir, "no-such-dir", "artifact")

	_, err := Export(request(staged, "node_modules", dest))

	require.ErrorIs(t, err, ErrUnsafePath)
	assert.Empty(t, hostDirEntries(t, hostDir))
}

func TestExport_ExceedingLimits_RefusedBeforeAnyHostWrite(t *testing.T) {
	tests := []struct {
		name   string
		limits Limits
	}{
		{name: "byte_ceiling", limits: Limits{MaxBytes: 8, MaxFiles: 1000}},
		{name: "file_ceiling", limits: Limits{MaxBytes: 1 << 20, MaxFiles: 2}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			staged := stagedFixture(t)
			hostDir, dest := destIn(t)
			req := request(staged, "node_modules", dest)
			req.Limits = tt.limits

			_, err := Export(req)

			require.ErrorIs(t, err, ErrLimitExceeded)
			assert.Empty(t, hostDirEntries(t, hostDir),
				"the limit must be enforced during planning, before any host write")
		})
	}
}

func TestExport_WithinLimits_Succeeds(t *testing.T) {
	staged := stagedFixture(t)
	_, dest := destIn(t)
	req := request(staged, "node_modules", dest)
	req.Limits = Limits{MaxBytes: 1 << 20, MaxFiles: 10}

	res, err := Export(req)

	require.NoError(t, err)
	assert.Equal(t, 4, res.Files)
}

func TestExport_DefaultLimits_AreApplied(t *testing.T) {
	got := Limits{}.withDefaults()
	assert.Equal(t, DefaultMaxBytes, got.MaxBytes)
	assert.Equal(t, DefaultMaxFiles, got.MaxFiles)
}

func TestExport_HardlinkLeavingTheSubtree_Refused(t *testing.T) {
	staged := stagedFixture(t)
	// A second link to an in-subtree file, planted OUTSIDE the subtree: the
	// exported artifact would alias a file the host never named.
	require.NoError(t, os.Link(
		filepath.Join(staged, "node_modules", "pkg", "index.js"),
		filepath.Join(staged, "src", "aliased.js")))

	hostDir, dest := destIn(t)
	_, err := Export(request(staged, "node_modules", dest))

	require.ErrorIs(t, err, ErrHardlinkEscape)
	assert.Empty(t, hostDirEntries(t, hostDir))
}

func TestExport_HardlinkFullyInsideTheSubtree_Allowed(t *testing.T) {
	staged := stagedFixture(t)
	require.NoError(t, os.Link(
		filepath.Join(staged, "node_modules", "pkg", "index.js"),
		filepath.Join(staged, "node_modules", "pkg", "index.copy.js")))

	_, dest := destIn(t)
	res, err := Export(request(staged, "node_modules", dest))

	require.NoError(t, err, "a hardlink whose every link is inside the subtree is contained")
	assert.Equal(t, 5, res.Files)
	got, err := os.ReadFile(filepath.Join(dest, "pkg", "index.copy.js")) //nolint:gosec // test-owned temp dir
	require.NoError(t, err)
	assert.Equal(t, "module.exports = 1\n", string(got))
}

func TestExport_IrregularFile_Refused(t *testing.T) {
	staged := stagedFixture(t)
	requireFIFO(t, filepath.Join(staged, "node_modules", "pipe"))

	hostDir, dest := destIn(t)
	_, err := Export(request(staged, "node_modules", dest))

	require.ErrorIs(t, err, ErrUnsafePath)
	assert.Empty(t, hostDirEntries(t, hostDir))
}

func TestExport_Manifest_CarriesProvenanceAndPerFileHashes(t *testing.T) {
	staged := stagedFixture(t)
	_, dest := destIn(t)

	res, err := Export(request(staged, "node_modules", dest))
	require.NoError(t, err)
	assert.Equal(t, dest+ManifestSuffix, res.ManifestPath)

	data, err := os.ReadFile(res.ManifestPath)
	require.NoError(t, err)
	var m Manifest
	require.NoError(t, json.Unmarshal(data, &m))

	assert.Equal(t, ManifestSchema, m.Schema)
	assert.Equal(t, "sbx-1", m.SandboxID)
	assert.Equal(t, "MGIT-73", m.TaskID)
	assert.Equal(t, "libkrun", m.Backend)
	assert.Equal(t, "sha256:"+strings.Repeat("a", 64), m.BaseDigest)
	assert.Equal(t, "node_modules", m.GuestPath)
	assert.Equal(t, dest, m.HostPath)
	assert.Equal(t, fixedNow, m.ExportedAt.UTC())
	assert.Equal(t, res.Files, m.FileCount)
	assert.Equal(t, res.Bytes, m.ByteCount)
	assert.Equal(t, res.TreeHash, m.TreeHash)

	byPath := map[string]ManifestEntry{}
	for _, e := range m.Entries {
		byPath[e.Path] = e
	}
	index := byPath["pkg/index.js"]
	assert.Equal(t, int64(len("module.exports = 1\n")), index.Size)
	assert.Len(t, index.SHA256, 64, "every exported file carries its SHA-256 (ADR-002)")
	assert.Equal(t, "0644", index.Mode)
	assert.Equal(t, "../pkg/index.js", byPath[".bin/pkg"].Symlink)
}

func TestExport_TreeHash_IsStableAcrossIdenticalTrees(t *testing.T) {
	first := stagedFixture(t)
	second := stagedFixture(t)
	_, destA := destIn(t)
	_, destB := destIn(t)

	a, err := Export(request(first, "node_modules", destA))
	require.NoError(t, err)
	b, err := Export(request(second, "node_modules", destB))
	require.NoError(t, err)

	assert.Equal(t, a.TreeHash, b.TreeHash,
		"the tree hash covers content, not the host location it landed at")
}

func TestExport_TreeHash_ChangesWithContent(t *testing.T) {
	staged := stagedFixture(t)
	_, destA := destIn(t)
	a, err := Export(request(staged, "node_modules", destA))
	require.NoError(t, err)

	mustWrite(t, filepath.Join(staged, "node_modules", "pkg", "index.js"), "module.exports = 2\n", 0o644)
	_, destB := destIn(t)
	b, err := Export(request(staged, "node_modules", destB))
	require.NoError(t, err)

	assert.NotEqual(t, a.TreeHash, b.TreeHash)
}

func TestExport_UnreadableStagedTree_RefusedWithoutPartialWrite(t *testing.T) {
	hostDir, dest := destIn(t)

	_, err := Export(request(filepath.Join(t.TempDir(), "no-such-staged-tree"), "build", dest))

	require.Error(t, err)
	assert.Empty(t, hostDirEntries(t, hostDir))
}

func TestExport_SingleFileOverTheByteCeiling_Refused(t *testing.T) {
	staged := stagedFixture(t)
	hostDir, dest := destIn(t)
	req := request(staged, "src/main.go", dest)
	req.Limits = Limits{MaxBytes: 2, MaxFiles: 10}

	_, err := Export(req)

	require.ErrorIs(t, err, ErrLimitExceeded)
	assert.Empty(t, hostDirEntries(t, hostDir))
}

func TestExport_SingleFileHardlinkedOutside_Refused(t *testing.T) {
	staged := stagedFixture(t)
	require.NoError(t, os.Link(
		filepath.Join(staged, "src", "main.go"),
		filepath.Join(staged, "src", "alias.go")))

	hostDir, dest := destIn(t)
	_, err := Export(request(staged, "src/main.go", dest))

	require.ErrorIs(t, err, ErrHardlinkEscape,
		"exporting one file of a hardlink pair leaves the other link outside the exported subtree")
	assert.Empty(t, hostDirEntries(t, hostDir))
}

func TestExport_RelativeHostDestination_Refused(t *testing.T) {
	staged := stagedFixture(t)

	_, err := Export(request(staged, "node_modules", "relative/dest"))

	require.ErrorIs(t, err, ErrUnsafePath)
}

func TestExport_SymlinkedIntermediateComponent_Refused(t *testing.T) {
	staged := stagedFixture(t)
	outside := t.TempDir()
	mustWrite(t, filepath.Join(outside, "loot.txt"), "host only\n", 0o600)
	require.NoError(t, os.Symlink(outside, filepath.Join(staged, "hop")))

	hostDir, dest := destIn(t)
	_, err := Export(request(staged, "hop/loot.txt", dest))

	require.ErrorIs(t, err, ErrUnsafePath,
		"a symlinked component anywhere in the guest path must not lead the export out of the worktree")
	assert.Empty(t, hostDirEntries(t, hostDir))
}

func TestExport_ByteCeilingBoundary_ExactFitSucceeds(t *testing.T) {
	staged := t.TempDir()
	mustWrite(t, filepath.Join(staged, "out", "a.txt"), "12345", 0o644)
	_, dest := destIn(t)
	req := request(staged, "out", dest)
	req.Limits = Limits{MaxBytes: 5, MaxFiles: 1}

	res, err := Export(req)

	require.NoError(t, err, "a transfer exactly at the ceiling is within it")
	assert.Equal(t, int64(5), res.Bytes)
}

func TestExport_SymlinkToANotYetExistingInSubtreePath_IsAllowed(t *testing.T) {
	staged := t.TempDir()
	mustWrite(t, filepath.Join(staged, "out", "a.txt"), "a\n", 0o644)
	require.NoError(t, os.Symlink("./generated-later.txt", filepath.Join(staged, "out", "pending")))

	_, dest := destIn(t)
	res, err := Export(request(staged, "out", dest))

	require.NoError(t, err, "a dangling link INSIDE the subtree is not an escape")
	assert.Equal(t, 2, res.Files)
	target, err := os.Readlink(filepath.Join(dest, "pending"))
	require.NoError(t, err)
	assert.Equal(t, "./generated-later.txt", target)
}

func TestExport_EmptyDirectory_IsExportedAsADirectory(t *testing.T) {
	staged := t.TempDir()
	require.NoError(t, os.MkdirAll(filepath.Join(staged, "out", "empty"), 0o750))
	mustWrite(t, filepath.Join(staged, "out", "a.txt"), "a\n", 0o644)

	_, dest := destIn(t)
	res, err := Export(request(staged, "out", dest))

	require.NoError(t, err)
	assert.Equal(t, 1, res.Files, "directories are implied by their entries, not counted as files")
	info, err := os.Stat(filepath.Join(dest, "empty"))
	require.NoError(t, err)
	assert.True(t, info.IsDir(), "an empty directory the guest built still lands")
}

func TestExport_UnreadableSubdirectory_RefusedWithoutPartialWrite(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("SKIP: root ignores directory permissions, so this refusal cannot be provoked")
	}
	staged := t.TempDir()
	locked := filepath.Join(staged, "out", "locked")
	mustWrite(t, filepath.Join(locked, "secret.txt"), "x\n", 0o600)
	require.NoError(t, os.Chmod(locked, 0o000))
	t.Cleanup(func() { _ = os.Chmod(locked, 0o750) }) //nolint:gosec // restoring a traversable test dir so cleanup can remove it

	hostDir, dest := destIn(t)
	_, err := Export(request(staged, "out", dest))

	require.Error(t, err, "an unreadable subtree must refuse the export rather than export part of it")
	assert.Empty(t, hostDirEntries(t, hostDir))
}

func TestExport_MissingInputs_Refused(t *testing.T) {
	staged := stagedFixture(t)
	tests := []struct {
		name string
		req  Request
	}{
		{name: "no_staged_tree", req: Request{GuestPath: "out", HostPath: "/tmp/x", Now: fixedNow}},
		{name: "no_host_path", req: Request{StagedTree: staged, GuestPath: "node_modules", Now: fixedNow}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := Export(tt.req)
			require.ErrorIs(t, err, ErrUnsafePath)
		})
	}
}

func TestExport_UnwritableHostParent_RefusedWithoutPartialWrite(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("SKIP: root ignores directory permissions, so this refusal cannot be provoked")
	}
	staged := stagedFixture(t)
	hostDir := t.TempDir()
	//nolint:gosec // deliberately readable-not-writable: that IS the condition under test
	require.NoError(t, os.Chmod(hostDir, 0o500))
	t.Cleanup(func() { _ = os.Chmod(hostDir, 0o750) }) //nolint:gosec // restoring a writable test dir so cleanup can remove it

	_, err := Export(request(staged, "node_modules", filepath.Join(hostDir, "artifact")))

	require.Error(t, err, "an export that cannot create its staging area must fail, not half-write")
	assert.Empty(t, hostDirEntries(t, hostDir))
}
