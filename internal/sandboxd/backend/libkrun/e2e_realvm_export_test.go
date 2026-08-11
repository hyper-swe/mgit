//go:build cgo && !vzf && (darwin || (linux && libkrun))

package libkrun

import (
	"context"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/hyper-swe/mgit/internal/model"
	"github.com/hyper-swe/mgit/internal/sandboxd/artifactexport"
	"github.com/hyper-swe/mgit/internal/sandboxd/backend/microvm"
	"github.com/hyper-swe/mgit/internal/sandboxd/provision"
	"github.com/hyper-swe/mgit/internal/sandboxd/staging"
	gitstore "github.com/hyper-swe/mgit/internal/store/git"
)

// REAL-VM artifact export (MGIT-73). A unit-green suite is not evidence in this
// subsystem: the thing under test is whether a HOST-side read of a virtiofs
// share sees exactly what a REAL guest wrote into it — modes, nested
// directories, symlinks and all — and whether the containment refusals hold
// against links a real guest really created through the share rather than ones
// a test fabricated host-side.
//
// It drives the PRODUCTION path end to end: libkrun.NewManager -> Launch (which
// stages the SEC-03 tree and boots a real microVM) -> the guest builds an
// artifact -> Manager.ExportArtifact reads it out. Refs: MGIT-73, SEC-03, ADR-011

// exportTaskID is the task the export e2e's sandbox is bound to.
const exportTaskID = "MGIT-73"

// exportFixture builds the host side of a real launch: a shared mgit repo with
// the task branch, and a SEPARATE materialized worktree for the sandbox to
// mount. They must be separate directories — a worktree that contains the
// shared store is refused at launch by the SEC-03 quarantine check, which is
// itself the correct behavior.
func exportFixture(t *testing.T) (repoRoot, worktree string) {
	t.Helper()
	clock := func() time.Time { return time.Now().UTC() }
	repoRoot = t.TempDir()
	repo, err := gitstore.Init(repoRoot, clock)
	if err != nil {
		t.Fatalf("init host repo: %v", err)
	}
	t.Cleanup(func() { _ = repo.Close() })
	base, err := repo.Head()
	if err != nil {
		t.Fatalf("head: %v", err)
	}
	if err := gitstore.NewBranchStore(repo).CreateBranch(context.Background(),
		&model.Branch{Name: model.TaskBranchName(exportTaskID), HeadCommit: base}); err != nil {
		t.Fatalf("create task branch: %v", err)
	}
	worktree = filepath.Join(t.TempDir(), "wt")
	if err := gitstore.NewWorktreeStore(repo).MaterializeBranchTo(
		context.Background(), model.TaskBranchName(exportTaskID), worktree); err != nil {
		t.Fatalf("materialize worktree: %v", err)
	}
	if err := os.WriteFile(filepath.Join(worktree, "seed.txt"), []byte("host work\n"), 0o600); err != nil {
		t.Fatalf("seed the worktree: %v", err)
	}
	return repoRoot, worktree
}

// exportGuestBase builds the artifact-building guest and pre-creates the
// worktree mount point inside it.
//
// The mount point must exist AHEAD of boot because the production VM config
// shares the guest root READ-ONLY (FR-17.17), so the guest cannot mkdir it —
// mounting OVER an existing directory needs no write access to the underlying
// filesystem, creating one does.
func exportGuestBase(t *testing.T, worktreePath string) string {
	t.Helper()
	root := buildGuestWorkload(t, "artifactguest")
	if err := os.MkdirAll(filepath.Join(root, worktreePath), 0o750); err != nil {
		t.Fatalf("pre-create the guest worktree mount point: %v", err)
	}
	return root
}

// launchExportSandbox boots a real microVM through the manager and waits for
// the guest to finish building its artifact. It returns the manager and the
// sandbox ID so the caller can drive the production export verb.
func launchExportSandbox(t *testing.T, repoRoot, worktree, guestRoot string) (mgr *microvm.Manager, sandboxID, staged string) {
	t.Helper()
	prov, perr := provision.NewStoreProvisioner(repoRoot)
	if perr != nil {
		t.Fatalf("provisioner: %v", perr)
	}
	workDir := shortTempDir(t)
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelWarn}))
	hv, err := NewHypervisor(logger)
	if err != nil {
		t.Fatalf("NewHypervisor: %v", err)
	}
	mgr, err = microvm.NewManager(microvm.Config{
		Backend: model.BackendLibkrun,
		WorkDir: workDir,
		Resolve: func(string) (microvm.ImagePaths, error) {
			return microvm.ImagePaths{RootfsPath: guestRoot}, nil
		},
		// ONE deviation from the production wiring, and it is about the test
		// fixture rather than the feature: the guest root here is a bare
		// directory holding a single static workload, not a real guest base
		// image, and libkrun will not boot such a root shared READ-ONLY (every
		// other real-VM test in this package shares its workload root rw for
		// the same reason). The worktree share — the thing this test is about —
		// is exactly the production one, staged by the real launch path.
		Hypervisor:       writableRootHypervisor{inner: hv},
		GuestDialer:      newGuestDialer(workDir),
		StoreProvisioner: prov,
		Logger:           logger,
		Clock:            func() time.Time { return time.Now().UTC() },
	})
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	info, err := mgr.Launch(ctx, model.SandboxLaunchOptions{
		TaskID: exportTaskID, WorktreePath: worktree,
		ImageRef: "artifactguest@sha256:" + strings.Repeat("b", 64),
		Network:  model.NetworkPolicy{Mode: model.NetworkModeNone},
		CPUs:     1, MemoryMB: 512,
	})
	if err != nil {
		t.Fatalf("Launch (real VM): %v", err)
	}
	t.Cleanup(func() { _ = mgr.Remove(context.Background(), info.ID, true) })
	stateDir := microvm.SandboxStateDir(workDir, info.ID)
	waitForGuestConsole(t, filepath.Join(stateDir, consoleLogName))
	return mgr, info.ID, filepath.Join(stateDir, stagingDirName)
}

// writableRootHypervisor shares the guest ROOT read-write; see the note at its
// only call site. Everything else — staging, the worktree share, the boot
// tokens, the export path — is the production configuration.
type writableRootHypervisor struct{ inner microvm.Hypervisor }

func (h writableRootHypervisor) CreateVM(cfg microvm.VMConfig) (microvm.VM, error) {
	cfg.RootfsReadOnly = false
	return h.inner.CreateVM(cfg)
}

// waitForGuestConsole blocks until the guest reports it has finished building,
// failing loudly (with the console) if it never does.
func waitForGuestConsole(t *testing.T, consolePath string) {
	t.Helper()
	deadline := time.Now().Add(60 * time.Second)
	for time.Now().Before(deadline) {
		data, err := os.ReadFile(consolePath) //nolint:gosec // test-owned state dir
		if err == nil {
			text := string(data)
			if strings.Contains(text, "GUEST: done") {
				for _, want := range []string{"GUEST-RESULT MOUNT = OK",
					"GUEST-RESULT BUILD = OK", "GUEST-RESULT ESCAPES = PLANTED"} {
					if !strings.Contains(text, want) {
						t.Fatalf("guest console missing %q; got:\n%s", want, text)
					}
				}
				t.Logf("guest console:\n%s", text)
				return
			}
			if strings.Contains(text, "krun_vm_failed") {
				t.Fatalf("the VM failed to boot; console:\n%s", text)
			}
		}
		time.Sleep(200 * time.Millisecond)
	}
	data, _ := os.ReadFile(consolePath) //nolint:gosec // test-owned state dir
	t.Fatalf("the guest never finished within the deadline; console:\n%s", data)
}

// TestE2E_Libkrun_RealVM_ArtifactExport_GuestBuiltTreeReachesTheHost is the
// happy path on hardware: a real guest builds a dependency tree inside its
// sandbox and the host-initiated export brings it out intact, with provenance.
// Refs: MGIT-73, ADR-011
func TestE2E_Libkrun_RealVM_ArtifactExport_GuestBuiltTreeReachesTheHost(t *testing.T) {
	requireRealVM(t)
	repoRoot, worktree := exportFixture(t)
	mgr, id, staged := launchExportSandbox(t, repoRoot, worktree, exportGuestBase(t, worktree))

	cache := t.TempDir()
	dest := filepath.Join(cache, "node_modules")
	res, err := mgr.ExportArtifact(context.Background(), id,
		model.ArtifactExportRequest{GuestPath: "node_modules", HostPath: dest})
	if err != nil {
		t.Fatalf("export the guest-built tree: %v", err)
	}

	if res.Files != 4 { // 3 regular files + the .bin symlink
		t.Errorf("exported %d entries, want 4", res.Files)
	}
	got, err := os.ReadFile(filepath.Join(dest, "pkg", "index.js")) //nolint:gosec // test-owned temp dir
	if err != nil || string(got) != "module.exports = 1\n" {
		t.Errorf("exported content = %q, err %v; want the bytes the guest wrote", got, err)
	}
	// The exported script must be EXECUTABLE, whichever honest route the host
	// had to the guest's mode. The two backends differ here and the difference
	// is real, not a bug in either: libkrun's macOS filesystem device gives
	// guest-created files PLACEHOLDER permission bits (the staged file's own
	// mode is 0600) and records the true st_mode in the share's stat attribute,
	// so the export must read the record; libkrun on LINUX presents the guest's
	// mode in the file's own bits, so a plain host stat is already the truth
	// and there is no record to read (measured on real KVM, MGIT-87).
	//
	// The assertion is therefore about the OUTCOME plus the ATTRIBUTION being
	// consistent with it, rather than about one platform's mechanism: pinning
	// "share-record" made this test assert a macOS implementation detail, and
	// it failed on Linux for a tree that had exported perfectly.
	// Refs: MGIT-81, MGIT-87
	src, err := os.Lstat(filepath.Join(staged, "node_modules", "pkg", "bin", "run.sh"))
	if err != nil {
		t.Fatalf("stat the staged source: %v", err)
	}
	fi, err := os.Lstat(filepath.Join(dest, "pkg", "bin", "run.sh"))
	if err != nil || fi.Mode().Perm() != 0o755 {
		t.Errorf("exported mode = %v, err %v; want the guest's 0755 (an exported .bin script "+
			"that is not executable defeats the cache this verb exists for)", fi.Mode().Perm(), err)
	}
	t.Logf("mode fidelity: guest wrote 0755, the staged file's own bits are %v, the export produced %v",
		src.Mode().Perm(), fi.Mode().Perm())
	// And the sidecar must say WHERE that mode was observed, so a consumer is
	// never left guessing whether an export invented it. An absent mode_source
	// means "a plain host stat" (ModeSourceHostStat), which is only an honest
	// answer when the staged file really does carry the mode that was exported.
	manifest, err := os.ReadFile(res.ManifestPath) //nolint:gosec // test-owned temp dir
	if err != nil {
		t.Fatalf("read the provenance sidecar: %v", err)
	}
	claimsRecord := strings.Contains(string(manifest), `"mode_source": "share-record"`)
	switch {
	case src.Mode().Perm() == 0o755 && claimsRecord:
		t.Errorf("the staged file already carries 0755, so the export must attribute "+
			"the mode to a host stat, not to a share record; got:\n%s", manifest)
	case src.Mode().Perm() != 0o755 && !claimsRecord:
		t.Errorf("the staged file's own bits are %v, so 0755 can only have come from "+
			"the share record — the sidecar must say so; got:\n%s", src.Mode().Perm(), manifest)
	}
	link, err := os.Readlink(filepath.Join(dest, ".bin", "run"))
	if err != nil || link != "../pkg/bin/run.sh" {
		t.Errorf("exported symlink = %q, err %v; want the guest's in-tree link", link, err)
	}
	if _, err := os.Stat(res.ManifestPath); err != nil {
		t.Errorf("no provenance sidecar at %s: %v", res.ManifestPath, err)
	}
	t.Logf("REAL VM PASS: exported %d entries / %d bytes (tree %s) with provenance at %s",
		res.Files, res.Bytes, res.TreeHash, res.ManifestPath)
}

// TestE2E_Libkrun_RealVM_ArtifactExport_HostileGuestIsRefused is the deny half
// on hardware: the escapes a REAL guest planted through the share are refused
// host-side with nothing written, a second export to the same destination is
// refused rather than overwriting, and the private store cannot be exported at
// all — so committed objects still cross only through land.
// Refs: MGIT-73, SEC-03, SEC-10, ADR-011
func TestE2E_Libkrun_RealVM_ArtifactExport_HostileGuestIsRefused(t *testing.T) {
	requireRealVM(t)
	repoRoot, worktree := exportFixture(t)
	mgr, id, _ := launchExportSandbox(t, repoRoot, worktree, exportGuestBase(t, worktree))

	cache := t.TempDir()
	// 1. The guest's escaping symlinks refuse the whole subtree, host-side.
	_, err := mgr.ExportArtifact(context.Background(), id,
		model.ArtifactExportRequest{GuestPath: "hostile", HostPath: filepath.Join(cache, "hostile")})
	if err == nil || !strings.Contains(err.Error(), staging.ErrSymlinkEscape.Error()) {
		t.Fatalf("a guest-planted escaping symlink must refuse the export; got %v", err)
	}
	if entries, rerr := os.ReadDir(cache); rerr != nil || len(entries) != 0 {
		t.Fatalf("a refused export wrote %v to the host (want nothing), err %v", entries, rerr)
	}

	// 2. The private store never leaves this way.
	_, err = mgr.ExportArtifact(context.Background(), id,
		model.ArtifactExportRequest{GuestPath: ".mgit", HostPath: filepath.Join(cache, "store")})
	if err == nil || !strings.Contains(err.Error(), artifactexport.ErrUnsafePath.Error()) {
		t.Fatalf("exporting the sandbox's private store must be refused; got %v", err)
	}

	// 3. A legitimate export succeeds, and repeating it REFUSES rather than
	//    overwriting (the documented collision policy).
	dest := filepath.Join(cache, "node_modules")
	if _, err := mgr.ExportArtifact(context.Background(), id,
		model.ArtifactExportRequest{GuestPath: "node_modules", HostPath: dest}); err != nil {
		t.Fatalf("the legitimate export must still succeed: %v", err)
	}
	before, err := os.ReadFile(filepath.Join(dest, "pkg", "index.js")) //nolint:gosec // test-owned temp dir
	if err != nil {
		t.Fatalf("read the exported artifact: %v", err)
	}
	_, err = mgr.ExportArtifact(context.Background(), id,
		model.ArtifactExportRequest{GuestPath: "node_modules", HostPath: dest})
	if err == nil || !strings.Contains(err.Error(), artifactexport.ErrCollision.Error()) {
		t.Fatalf("a colliding export must be refused; got %v", err)
	}
	after, err := os.ReadFile(filepath.Join(dest, "pkg", "index.js")) //nolint:gosec // test-owned temp dir
	if err != nil || string(after) != string(before) {
		t.Errorf("the refused collision changed the existing host artifact")
	}
	t.Log("REAL VM PASS: guest-planted escapes, the private store and a collision are all refused host-side")
}
