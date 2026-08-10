//go:build cgo && !vzf && (darwin || (linux && libkrun))

package libkrun

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/hyper-swe/mgit/internal/model"
	"github.com/hyper-swe/mgit/internal/sandboxd/backend/microvm"
	"github.com/hyper-swe/mgit/internal/sandboxd/guestexec"
	"github.com/hyper-swe/mgit/internal/sandboxd/provision"
	"github.com/hyper-swe/mgit/internal/sandboxd/worktreesync"
)

// Host->guest worktree sync, asserted through a LIVE virtiofs mount.
//
// WHY THIS FILE EXISTS. MGIT-71's sync coverage lives in
// worktreesync/syncer_test.go and is labeled, accurately, "the HyperSwe repro
// at the unit layer": it writes to a plain host directory with no VM anywhere.
// That cannot see the failure mode which actually cost us the feature — a
// host-side RENAME is invisible to a guest holding the directory over
// virtiofs, while every host-side assertion about the same tree passes. The
// tree is correct and the guest still reads the old bytes. Only a booted VM
// with the share mounted can distinguish those two worlds, so until now
// nothing in the suite could.
//
// The sync verb (MGIT-76) is the first caller a user drives directly, which
// makes the gap load-bearing rather than theoretical: a regression here
// reports success and serves stale code to the agent.
//
// Refs: MGIT-76, MGIT-71, SEC-03, ADR-011

// stagedTreeDirName mirrors microvm's unexported stagedTreeName. The
// duplication is deliberate and guarded by
// TestE2E_Libkrun_RealVM_Sync_StagedTreeNameMatchesTheBackend below: this
// package cannot import the constant, and a silent drift would make every
// assertion in this file target a directory the guest never sees — which
// would look exactly like a pass.
const stagedTreeDirName = "worktree-staging"

// buildGuestSupervisor lays down a guest root running the REAL mgit-guest as
// PID 1, which is what gives these tests a vsock exec channel to read the
// guest's own view of the tree through.
func buildGuestSupervisor(t *testing.T) string {
	t.Helper()
	guestRoot := t.TempDir()
	// mgit-guest is PID 1 and mounts into these, so the base tree must
	// provide them — the same real requirement the control-plane e2e documents.
	for _, d := range append([]string{"sbin"}, guestBaseDirs...) {
		if err := os.MkdirAll(filepath.Join(guestRoot, d), 0o750); err != nil {
			t.Fatal(err)
		}
	}
	// The FHS directories are NOT decoration, and their absence is why an
	// earlier version of this test could only boot with a writable root.
	// Production shares the root READ-ONLY (vmConfig hardcodes it, FR-17.17),
	// and mgit-guest's writable-root overlay + switch_root cannot create what
	// the read-only lower does not already contain. A base missing them dies
	// before its first log line — the console carries the host's "vm entering"
	// record and nothing else — so a test root that skips them fails in a way
	// that looks like a broken sync rather than a malformed base. A real base
	// (`mgit sandbox base from <oci-image>`) always has them.
	// Refs: FR-17.17, MGIT-76
	for _, d := range []string{"etc", "usr", "var", "run", "sys", "root", "home", "lib", "opt"} {
		if err := os.MkdirAll(filepath.Join(guestRoot, d), 0o750); err != nil {
			t.Fatal(err)
		}
	}
	//nolint:gosec // G204: fixed argv; output path is a t.TempDir
	build := exec.Command("go", "build", "-trimpath", "-buildvcs=false",
		"-ldflags=-buildid=", "-o", filepath.Join(guestRoot, guestInitPath), "./cmd/mgit-guest")
	build.Dir = repoRoot(t)
	build.Env = append(os.Environ(), "GOOS=linux", "GOARCH="+runtime.GOARCH, "CGO_ENABLED=0")
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build mgit-guest: %v\n%s", err, out)
	}
	buildGuestCat(t, guestRoot)
	return guestRoot
}

// buildGuestCat installs a minimal /bin/cat into the guest root.
//
// The guest base these tests boot is deliberately minimal — mgit-guest and
// nothing else — so there is no coreutils to read a file with. Reading has to
// happen INSIDE the guest (a host-side read of the shared directory proves
// nothing about what the guest observes through virtiofs), so the guest needs
// its own reader.
func buildGuestCat(t *testing.T, guestRoot string) {
	t.Helper()
	src := filepath.Join(t.TempDir(), "main.go")
	const prog = `package main

import (
	"os"
)

func main() {
	b, err := os.ReadFile(os.Args[1])
	if err != nil {
		os.Stderr.WriteString(err.Error())
		os.Exit(1)
	}
	os.Stdout.Write(b)
}
`
	if err := os.WriteFile(src, []byte(prog), 0o600); err != nil {
		t.Fatal(err)
	}
	out := filepath.Join(guestRoot, "bin", "cat")
	if err := os.MkdirAll(filepath.Dir(out), 0o750); err != nil {
		t.Fatal(err)
	}
	//nolint:gosec // G204: fixed argv; both paths are t.TempDir paths
	build := exec.Command("go", "build", "-trimpath", "-o", out, src)
	build.Env = append(os.Environ(), "GOOS=linux", "GOARCH="+runtime.GOARCH, "CGO_ENABLED=0")
	if o, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build guest cat: %v\n%s", err, o)
	}
}

// guestCat reads one file from inside the running guest over the production
// vsock exec path. Reading through the GUEST is the entire point: a host-side
// read of the same path proves nothing about what the guest observes.
func guestCat(t *testing.T, workDir, sandboxID, path string) string {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	conn, err := newGuestDialer(workDir).DialGuest(ctx, sandboxID)
	if err != nil {
		t.Fatalf("host could not reach the guest exec channel: %v", err)
	}
	defer func() { _ = conn.Close() }()

	var stdout, stderr bytes.Buffer
	if _, err := guestexec.Run(conn, model.ExecRequest{
		Command: []string{"/bin/cat", path},
	}, &stdout, &stderr); err != nil {
		t.Fatalf("guest cat %s: %v (stderr=%q)", path, err, stderr.String())
	}
	return strings.TrimSpace(stdout.String())
}

// syncSandbox is one real microVM launched through the PRODUCTION path, with
// everything a sync assertion needs to address it.
type syncSandbox struct {
	mgr      *microvm.Manager
	id       string
	workDir  string
	worktree string // the HOST worktree the sandbox was launched against
	staged   string // the staged tree the guest actually mounts
}

// launchRealVMForSync boots one sandbox through microvm.Manager.Launch — not
// through the hypervisor directly.
//
// THE LAYER IS THE POINT, and a previous version of this file got it wrong.
// Manager.Launch is what calls worktreesync.RecordDelivery, and that delivery
// baseline is what lets a later sync tell a host edit from a guest one.
// Booting via the hypervisor leaves no baseline, under which every staged path
// legitimately looks guest-created and the very first sync refuses. The
// tempting repair — have the test record its own baseline — would be
// scaffolding doing production's job: it would keep passing if Launch ever
// stopped recording one, and "sync refuses on first use for every sandbox" is
// precisely the regression worth catching.
//
// Manager is also the entry the CLI verb reaches (service -> daemon ->
// SyncWorktree), so driving it covers the verb rather than the library
// beneath it. Refs: MGIT-76, MGIT-71, ADR-011
func launchRealVMForSync(t *testing.T, sandboxID, taskID string) syncSandbox {
	t.Helper()
	guestRoot := buildGuestSupervisor(t)
	// The manager provisions the SEC-03 private store itself, from the same
	// production provisioner — so only the project and its worktree are
	// seeded here.
	project, worktree := seedProjectWithLinkedWorktree(t, taskID)

	workDir := shortTempDir(t)
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelWarn}))
	hv, err := NewHypervisor(logger)
	if err != nil {
		t.Fatalf("NewHypervisor: %v", err)
	}
	prov, err := provision.NewStoreProvisioner(project)
	if err != nil {
		t.Fatalf("store provisioner: %v", err)
	}
	mgr, err := microvm.NewManager(microvm.Config{
		Backend: model.BackendLibkrun,
		WorkDir: workDir,
		// libkrun boots libkrunfw's kernel and shares a DIRECTORY as the guest
		// root, so resolution here is the guest root and nothing else.
		Resolve: func(string) (microvm.ImagePaths, error) {
			return microvm.ImagePaths{RootfsPath: guestRoot}, nil
		},
		Hypervisor:       hv,
		GuestDialer:      newGuestDialer(workDir),
		StoreProvisioner: prov,
		Logger:           logger,
		Clock:            time.Now,
	})
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	info, err := mgr.Launch(ctx, model.SandboxLaunchOptions{
		SandboxID:    sandboxID,
		TaskID:       taskID,
		WorktreePath: worktree,
		ImageRef:     "guest-base@sha256:" + strings.Repeat("a", 64),
		Network:      model.NetworkPolicy{Mode: model.NetworkModeNone},
		CPUs:         1,
		MemoryMB:     512,
	})
	if err != nil {
		t.Fatalf("Manager.Launch (real VM): %v", err)
	}
	t.Cleanup(func() { _ = mgr.Remove(context.Background(), info.ID, true) })

	stateDir := microvm.SandboxStateDir(workDir, info.ID)
	waitGuestBooted(t, stateDir, "mgit-guest")
	staged := filepath.Join(stateDir, stagedTreeDirName)
	if !microvm.HasStagedTree(stateDir) {
		t.Fatalf("the launch left no staged tree at %s; every assertion below "+
			"would target a directory the guest never mounts", staged)
	}
	return syncSandbox{mgr: mgr, id: info.ID, workDir: workDir, worktree: worktree, staged: staged}
}

// seedProjectWithLinkedWorktree builds a real mgit project with one commit for
// taskID and a LINKED worktree bound to that task, returning both.
//
// The linked layout is not incidental. A sandbox is launched against a
// worktree, and SEC-03 requires the shared object store to live OUTSIDE the
// mounted worktree — otherwise the guest would see it as an ordinary file and
// Manager.Launch fails closed with ErrSharedStoreReachable. A repo whose root
// IS the worktree (its .mgit inside) therefore cannot boot through the
// production path at all; the earlier version of this file only got away with
// it by bypassing the manager. This is the layout `mgit work` produces and the
// one HyperSwe runs. Refs: SEC-03, FR-16, MGIT-76
func seedProjectWithLinkedWorktree(t *testing.T, taskID string) (project, worktree string) {
	t.Helper()
	project = t.TempDir()
	// The worktree must sit outside the project, so the project's .mgit — the
	// shared store — is not inside the mount handed to the guest.
	worktree = filepath.Join(t.TempDir(), "wt")

	mgitBin := filepath.Join(t.TempDir(), "mgit-host")
	build := exec.Command("go", "build", "-o", mgitBin, "./cmd/mgit") //nolint:gosec // fixed argv
	build.Dir = repoRoot(t)
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build host mgit: %v\n%s", err, out)
	}
	run := func(dir string, args ...string) {
		cmd := exec.Command(mgitBin, args...) //nolint:gosec // test-built binary
		cmd.Dir = dir
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("mgit %v: %v\n%s", args, err, out)
		}
	}
	run(project, "init")
	if err := os.WriteFile(filepath.Join(project, "seed.txt"), []byte("host work\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	run(project, "add", "seed.txt")
	run(project, "commit", "-m", "host work before the sandbox", "--task", taskID)
	run(project, "worktree", "add", worktree, "--task-id", taskID)
	return project, worktree
}

// waitGuestBooted blocks until the guest's console reports ready.
//
// Manager.Launch returns as soon as the VMM is up, which is ~1s before the
// guest userspace has bound its vsock port. Distinct from Manager.Exec's own
// readiness wait: these tests read through the raw dialer deliberately (see
// guestRead), so they need their own.
func waitGuestBooted(t *testing.T, stateDir, ready string) {
	t.Helper()
	consolePath := filepath.Join(stateDir, consoleLogName)
	deadline := time.Now().Add(45 * time.Second)
	for time.Now().Before(deadline) {
		data, err := os.ReadFile(consolePath) //nolint:gosec // test-owned state dir
		if err == nil {
			if strings.Contains(string(data), ready) {
				t.Logf("guest console:\n%s", data)
				return
			}
			if strings.Contains(string(data), "krun_vm_failed") {
				t.Fatalf("the VM failed to boot; console:\n%s", data)
			}
		}
		time.Sleep(200 * time.Millisecond)
	}
	data, _ := os.ReadFile(consolePath) //nolint:gosec // test-owned state dir
	t.Fatalf("guest never reported %q; console:\n%s", ready, data)
}

// guestRead reads one path through the guest, over the RAW exec dialer rather
// than Manager.Exec.
//
// Manager.Exec syncs before every command, by design. Reading through it would
// therefore deliver the host edit as a side effect of observing it, making
// every assertion below vacuous — the "before" read would already show V2, and
// the conflict case would be refused by the exec rather than by the verb under
// test. Refs: MGIT-76
func guestRead(t *testing.T, s syncSandbox, path string) string {
	t.Helper()
	return guestCat(t, s.workDir, s.id, path)
}

// hostSync drives the production verb: the same Manager.SyncWorktree the CLI
// and the MCP tool reach.
func hostSync(t *testing.T, s syncSandbox, opts model.WorktreeSyncOptions) (*model.WorktreeSyncReport, error) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	return s.mgr.SyncWorktree(ctx, s.id, opts)
}

// TestE2E_Libkrun_RealVM_Sync_HostEditReachesTheRunningGuest is the assertion
// MGIT-71 shipped without and MGIT-76 needs: after a host edit and a
// production sync, a guest with the tree ALREADY MOUNTED reads the new bytes.
//
// The "before" read is not ceremony. Without it a test that never delivered
// anything — a broken share, a wrong staged path — would still see V2 sitting
// on the host and pass. Reading V1 through the guest first proves the mount is
// live and the assertion is about the sync rather than about the setup.
//
// Refs: MGIT-76, MGIT-71
func TestE2E_Libkrun_RealVM_Sync_HostEditReachesTheRunningGuest(t *testing.T) {
	requireRealVM(t)

	sb := launchRealVMForSync(t, "syncrvm", "MGIT-76")
	appPath := filepath.Join(sb.worktree, "seed.txt")

	// The guest is mounted at the worktree's IDENTICAL path.
	if got := guestRead(t, sb, appPath); got != "host work" {
		t.Fatalf("precondition: guest must see the delivered content before the "+
			"sync, got %q (a failure here means the share is not live, so the "+
			"rest of this test would prove nothing)", got)
	}

	// An unchanged worktree must be a genuine no-op that SAYS so, rather than
	// re-staging and reporting phantom work (acceptance 4).
	noop, err := hostSync(t, sb, model.WorktreeSyncOptions{})
	if err != nil {
		t.Fatalf("sync of an unchanged worktree must succeed: %v", err)
	}
	if !noop.Skipped || noop.Changed() {
		t.Fatalf("an unchanged worktree must report a skip and change nothing, got %+v", noop)
	}

	// Host edits after boot, then the PRODUCTION verb.
	if err := os.WriteFile(appPath, []byte("V2"), 0o600); err != nil {
		t.Fatal(err)
	}

	// A dry run must classify WITHOUT touching the guest (acceptance 3): the
	// guest still reads the old bytes afterwards.
	dry, err := hostSync(t, sb, model.WorktreeSyncOptions{DryRun: true})
	if err != nil {
		t.Fatalf("dry run: %v", err)
	}
	if !dry.DryRun || len(dry.Updated) == 0 {
		t.Fatalf("a dry run over a changed worktree must classify the change and "+
			"record that it applied nothing, got %+v", dry)
	}
	if got := guestRead(t, sb, appPath); got != "host work" {
		t.Fatalf("a dry run delivered content into the guest (%q) — the whole "+
			"point of --dry-run is that HyperSwe can classify without an exec "+
			"and without a change", got)
	}

	res, err := hostSync(t, sb, model.WorktreeSyncOptions{})
	if err != nil {
		t.Fatalf("sync into a running guest: %v", err)
	}
	if len(res.Updated) == 0 {
		t.Fatalf("sync reported nothing updated; a sync that reports no work "+
			"while the host changed is the silent-staleness failure: %+v", res)
	}

	// THE ASSERTION: the guest, not the host, observes the new content.
	if got := guestRead(t, sb, appPath); got != "V2" {
		t.Fatalf("guest still reads %q after a successful sync — the host tree "+
			"is correct and the guest is stale, which is exactly the virtiofs "+
			"failure a host-side assertion cannot see", got)
	}
	t.Logf("REAL VM PASS: no-op reported, dry run changed nothing, then host edit -> " +
		"sync -> guest reads V2 through a live virtiofs mount")
}

// TestE2E_Libkrun_RealVM_Sync_RefusesAConflictAndStillDeliversAfterward pairs
// the refusal with its positive control.
//
// A refusal on its own is worthless evidence: a sync that is simply broken
// also refuses everything. The second half proves the first was a DECISION —
// the same sandbox, the same code path, delivering successfully once the
// conflict is resolved. Refs: MGIT-76
func TestE2E_Libkrun_RealVM_Sync_RefusesAConflictAndStillDeliversAfterward(t *testing.T) {
	requireRealVM(t)

	sb := launchRealVMForSync(t, "syncconf", "MGIT-76")
	appPath := filepath.Join(sb.worktree, "seed.txt")

	// Diverge both sides. The guest-side edit is written into the staged tree
	// — the directory the guest owns and mounts read-write, so a write there is
	// indistinguishable from one the guest made itself; the guest read below
	// confirms it observes exactly this content. The host edits the same path.
	// This is the both-sides-changed class.
	if err := os.WriteFile(filepath.Join(sb.staged, "seed.txt"), []byte("GUEST"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(appPath, []byte("HOST"), 0o600); err != nil {
		t.Fatal(err)
	}
	report, err := hostSync(t, sb, model.WorktreeSyncOptions{})
	if err == nil {
		t.Fatal("a both-sides-changed path must refuse; silently overwriting " +
			"the guest's edit is data loss inside the sandbox")
	}
	if !errors.Is(err, worktreesync.ErrConflict) {
		t.Fatalf("the refusal must be classifiable as a conflict, not generic failure: %v", err)
	}
	// The refusal must NAME the path, which is the report HyperSwe cannot get
	// today without attempting an exec (acceptance 1 and 3).
	if report == nil || !report.Refused || len(report.Conflicts) == 0 {
		t.Fatalf("a refusal must carry the classification that caused it, got %+v", report)
	}
	if report.Conflicts[0].Path != "seed.txt" {
		t.Fatalf("the refusal must name the conflicting path, got %+v", report.Conflicts)
	}
	t.Logf("REAL VM: conflict refused as designed, naming %q: %v", report.Conflicts[0].Path, err)

	// The guest keeps ITS content — a refused sync must change nothing.
	if got := guestRead(t, sb, appPath); got != "GUEST" {
		t.Fatalf("a refused sync must leave the guest untouched, got %q", got)
	}

	// POSITIVE CONTROL: resolve the divergence and the very same path delivers.
	//
	// Resolving means the GUEST side stops diverging from what was delivered —
	// the shape of "the guest's work was landed, or discarded". Writing some
	// third value into the guest tree would not resolve anything: the
	// classification is against the delivery baseline, not against the host.
	if err := os.WriteFile(filepath.Join(sb.staged, "seed.txt"), []byte("host work\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(appPath, []byte("V3"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := hostSync(t, sb, model.WorktreeSyncOptions{}); err != nil {
		t.Fatalf("positive control failed — sync refuses even without a "+
			"conflict, so the refusal above proved nothing: %v", err)
	}
	if got := guestRead(t, sb, appPath); got != "V3" {
		t.Fatalf("positive control: guest must read V3, got %q", got)
	}
	t.Logf("REAL VM PASS: conflict refused, then the same path delivered — the refusal was a decision")
}

// TestE2E_Libkrun_RealVM_Sync_StagedTreeNameMatchesTheBackend guards the
// constant duplicated at the top of this file. If microvm renames its staged
// tree, every sync assertion here would target a directory the guest does not
// mount and would still pass, because the host-side writes would succeed. This
// makes that drift fail loudly instead. Refs: MGIT-76
func TestE2E_Libkrun_RealVM_Sync_StagedTreeNameMatchesTheBackend(t *testing.T) {
	dir := t.TempDir()
	staged := filepath.Join(dir, stagedTreeDirName)
	if err := os.MkdirAll(staged, 0o700); err != nil {
		t.Fatal(err)
	}
	// microvm reports a staged tree only when the directory it expects exists,
	// so agreement here is proof the names still match.
	if !microvm.HasStagedTree(dir) {
		t.Fatalf("microvm does not recognize %q as its staged tree; the "+
			"constant in this file has drifted from the backend, and every "+
			"sync assertion here would silently target the wrong directory",
			stagedTreeDirName)
	}
}
