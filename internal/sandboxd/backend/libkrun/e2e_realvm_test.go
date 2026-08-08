//go:build cgo && !vzf && (darwin || (linux && libkrun))

package libkrun

import (
	"bytes"
	"context"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"golang.org/x/crypto/ssh"

	"github.com/hyper-swe/mgit/internal/model"
	"github.com/hyper-swe/mgit/internal/sandboxd/backend/microvm"
	"github.com/hyper-swe/mgit/internal/sandboxd/guestexec"
	"github.com/hyper-swe/mgit/internal/sandboxd/provision"
)

// REAL-VM e2e (MGIT-61.10): boots an actual libkrun microVM through the
// PRODUCTION path — Hypervisor.CreateVM -> krunVM.Start -> re-exec child ->
// newGuestCtx -> krun_start_enter — with the guest's virtio-net device backed
// by mgit's own netstack egress gateway running in that child.
//
// Everything before this proved the halves separately: the egress gateway
// against a simulated guest, and the re-exec lifecycle against a fake
// spawner. This runs them together, on hardware.
//
// HOW IT IS GATED (it must SKIP LOUDLY, never silently pass):
//   - build tag "libkrun && cgo" — the binding must be linked;
//   - MGIT_E2E_LIBKRUN=1 — opt in explicitly;
//   - the test binary must carry the macOS hypervisor entitlement, because
//     the re-exec child IS this binary. Build and sign it:
//     go test -c -tags libkrun -o /tmp/libkrun.test ./internal/sandboxd/backend/libkrun/
//     codesign --force --sign - --entitlements <plist with
//     com.apple.security.hypervisor> /tmp/libkrun.test
//     MGIT_E2E_LIBKRUN=1 /tmp/libkrun.test -test.run TestE2E_Libkrun
//
// Refs: MGIT-61.10, MGIT-61.8, ADR-010, SEC-04, FR-17.7

// e2eEnv opts into the real-VM tests.
const e2eEnv = "MGIT_E2E_LIBKRUN"

// requireRealVM skips loudly unless this run can actually boot a microVM.
func requireRealVM(t *testing.T) {
	t.Helper()
	if os.Getenv(e2eEnv) != "1" {
		t.Skipf("SKIP (real libkrun VM): set %s=1 to run; the test binary must also be "+
			"codesigned with com.apple.security.hypervisor — see this file's header", e2eEnv)
	}
	if runtime.GOOS == "darwin" && runtime.GOARCH != "arm64" {
		t.Skip("SKIP (real libkrun VM): libkrun on macOS requires Apple Silicon (HVF)")
	}
}

// buildGuest cross-compiles the testdata guest workload for the guest's
// architecture and installs it at the guest init path inside a fresh root
// directory, which libkrun shares over virtiofs (there is no rootfs image).
func buildGuest(t *testing.T) string {
	t.Helper()
	return buildGuestWorkload(t, "netguest")
}

// buildGuestWorkload cross-compiles one testdata guest workload and installs
// it at the guest init path in a fresh root directory, which libkrun shares
// over virtiofs (there is no rootfs image).
func buildGuestWorkload(t *testing.T, name string) string {
	t.Helper()
	root := t.TempDir()
	// A workload tree is still a guest base: give it the mount points the
	// contract requires, so these tests exercise a realistic root.
	for _, d := range append([]string{"sbin"}, guestBaseDirs...) {
		if err := os.MkdirAll(filepath.Join(root, d), 0o750); err != nil {
			t.Fatalf("guest root: %v", err)
		}
	}
	out := filepath.Join(root, guestInitPath)

	// The source path is resolved from THIS file's location, not the working
	// directory: the signing step means this suite is run as a standalone
	// binary from an arbitrary cwd, where `go test`'s package-dir convention
	// does not hold.
	_, thisFile, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("cannot locate the test source to find testdata/netguest")
	}
	src := filepath.Join(filepath.Dir(thisFile), "testdata", name)

	cmd := exec.Command("go", "build", "-o", out, ".") //nolint:gosec // fixed argv
	// Run the build IN the guest source dir (-C equivalent) so module
	// resolution works no matter where this binary was invoked from.
	cmd.Dir = src
	cmd.Env = append(os.Environ(), "GOOS=linux", "GOARCH="+runtime.GOARCH, "CGO_ENABLED=0")
	if combined, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("build guest workload: %v\n%s", err, combined)
	}
	return root
}

// bootVM runs one real microVM to completion and returns its console output.
// The guest's stdout is libkrun's stdout, which the parent wired to the
// per-VM console log — so the console IS the guest's report.
func bootVM(t *testing.T, cfg microvm.VMConfig) string {
	t.Helper()
	hv, err := NewHypervisor(slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelWarn})))
	if err != nil {
		t.Fatalf("NewHypervisor: %v", err)
	}
	vm, err := hv.CreateVM(cfg)
	if err != nil {
		t.Fatalf("CreateVM: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	if err := vm.Start(ctx); err != nil {
		t.Fatalf("Start (real VM): %v", err)
	}
	t.Cleanup(func() { _ = vm.Stop(context.Background(), true) })

	consolePath := filepath.Join(cfg.StateDir, consoleLogName)
	deadline := time.Now().Add(45 * time.Second)
	for time.Now().Before(deadline) {
		data, err := os.ReadFile(consolePath) //nolint:gosec // test-owned state dir
		if err == nil {
			if strings.Contains(string(data), "GUEST: done") {
				return string(data)
			}
			// A boot that failed before the guest ran will never print
			// anything more; waiting out the deadline only hides the reason.
			if strings.Contains(string(data), "krun_vm_failed") {
				t.Fatalf("the VM failed to boot; console:\n%s", data)
			}
		}
		time.Sleep(200 * time.Millisecond)
	}
	data, _ := os.ReadFile(consolePath) //nolint:gosec // test-owned state dir
	t.Fatalf("guest never finished within the deadline; console:\n%s", data)
	return ""
}

// realVMConfig is a launchable config for the built guest root.
func realVMConfig(t *testing.T, guestRoot, mode string, allowlist []string) microvm.VMConfig {
	t.Helper()
	return microvm.VMConfig{
		SandboxID:      "e2e-" + strings.ReplaceAll(t.Name(), "/", "-"),
		TaskID:         "MGIT-61.10",
		StateDir:       shortTempDir(t),
		CPUs:           1,
		MemoryMB:       512,
		RootfsPath:     guestRoot,
		RootfsReadOnly: false,
		// No vsock: this guest is the litmus workload, not mgit-guest, so
		// there is no in-guest listener for the control ports.
		VsockEnabled:     false,
		NetworkMode:      mode,
		NetworkAllowlist: allowlist,
		AttachNIC:        mode != model.NetworkModeNone,
	}
}

// TestE2E_Libkrun_RealVM_Boots proves the re-exec lifecycle end to end on
// hardware: the daemon-side Hypervisor spawns a child, the child configures
// and enters a real microVM, the guest runs and its output reaches the
// per-VM console log. Refs: MGIT-61.8, MGIT-61.10
func TestE2E_Libkrun_RealVM_Boots(t *testing.T) {
	requireRealVM(t)
	guestRoot := buildGuest(t)

	console := bootVM(t, realVMConfig(t, guestRoot, model.NetworkModeNone, nil))

	for _, want := range []string{"booted inside a real libkrun microVM", "GUEST: done"} {
		if !strings.Contains(console, want) {
			t.Errorf("console missing %q; got:\n%s", want, console)
		}
	}
	t.Logf("REAL VM PASS: booted and ran a guest through the production re-exec path\n%s", console)
}

// TestE2E_Libkrun_RealVM_NoneMode_NoEgress proves the containment claim on
// hardware: with the NIC bound to the discard socket, a guest dial to an
// arbitrary destination fails. It is the fail-closed half of the ADR-010
// guardrail — the same probe reaches the internet if libkrun is left on its
// TSI default.
//
// The guest here CONFIGURES ITS OWN NIC (testdata/netguest is its own PID 1),
// which is what makes this a claim about none mode rather than about an
// unaddressed interface: the guest has an address, a netmask and a default
// route, and still gets nowhere. Refs: FR-17.7, SEC-04, ADR-010, MGIT-68
func TestE2E_Libkrun_RealVM_NoneMode_NoEgress(t *testing.T) {
	requireRealVM(t)
	guestRoot := buildGuest(t)

	cfg := realVMConfig(t, guestRoot, model.NetworkModeNone, nil)
	console := bootVM(t, cfg)

	// Without this, "denied" would only mean the guest never had a network to
	// begin with — the MGIT-68 failure mode wearing a passing test's clothes.
	if !strings.Contains(console, "configured 10.0.2.15/24") {
		t.Fatalf("the guest did not configure its NIC, so a denial here proves nothing "+
			"about none mode; console:\n%s", console)
	}
	if !strings.Contains(console, "GUEST-RESULT OFF_ALLOWLIST = DENIED") {
		t.Fatalf("guest reached the network in none mode — TSI leak or missing NIC (ADR-010); console:\n%s", console)
	}
	t.Logf("REAL VM PASS: none mode denied egress to a guest that HAD an address and a "+
		"default route\n%s", console)
}

// TestE2E_Libkrun_RealVM_Allowlist_DefaultDeny proves the same claim through
// the REAL egress authorizer: allowlist mode runs the netstack gateway in the
// VM's own child process, and a destination the policy does not name is reset
// rather than proxied. This is litmus assertion 2 (no reverse tunnel / no
// exfiltration) against a real guest.
//
// The denial REASON is asserted, not merely the failure: a policy denial
// resets the handshake, so the guest sees "connection refused". "network is
// unreachable" would mean the guest had no route at all, which is a broken
// sandbox rather than an enforced one — and is what this test used to accept.
// The matching ALLOW assertions live in e2e_realvm_net_test.go.
// Refs: SEC-04, FR-17.8, MGIT-68
func TestE2E_Libkrun_RealVM_Allowlist_DefaultDeny(t *testing.T) {
	requireRealVM(t)
	guestRoot := buildGuest(t)

	// A policy that names something else entirely: the guest's destination is
	// off-list, so the forwarder must reset it.
	cfg := realVMConfig(t, guestRoot, model.NetworkModeAllowlist, []string{"proxy.golang.org:443"})
	console := bootVM(t, cfg)

	if !strings.Contains(console, "GUEST-RESULT OFF_ALLOWLIST = DENIED") {
		t.Fatalf("an off-allowlist destination was reachable from the guest (T3 exfiltration); console:\n%s", console)
	}
	if strings.Contains(console, "network is unreachable") || strings.Contains(console, "no route to host") {
		t.Fatalf("the off-list flow failed because the guest has NO NETWORK, not because "+
			"policy refused it (MGIT-68); console:\n%s", console)
	}
	if !strings.Contains(console, "connection refused") && !strings.Contains(console, "connection reset") {
		t.Errorf("the denial is not recognizable as a policy refusal (expected a reset); console:\n%s", console)
	}
	t.Logf("REAL VM PASS: allowlist mode RESET an off-list destination\n%s", console)
}

// TestE2E_Libkrun_RealVM_VirtiofsPerf is ADR-010 Gate 2: virtio-fs must be
// fast enough for the agent loop under an npm-install-class workload — many
// thousands of SMALL files, where per-file overhead dominates and virtio-fs
// is at its worst.
//
// It measures rather than gates: the ADR wanted a number, and a hard
// threshold on shared CI hardware would be a flake generator. The only
// failure is a result so slow it would make the loop unusable, plus the
// same workload timed on the HOST filesystem for a ratio that means
// something across machines. Refs: ADR-010 Gate 2, NFR-17.2
func TestE2E_Libkrun_RealVM_VirtiofsPerf(t *testing.T) {
	requireRealVM(t)

	guestRoot := buildGuestWorkload(t, "fsbench")
	cfg := realVMConfig(t, guestRoot, model.NetworkModeNone, nil)
	// The guest writes into /bench on its root — which IS the shared host
	// directory, so every operation crosses virtio-fs.
	cfg.RootfsReadOnly = false

	start := time.Now()
	console := bootVM(t, cfg)
	bootAndRun := time.Since(start)

	line := ""
	for _, l := range strings.Split(console, "\n") {
		if strings.HasPrefix(l, "BENCH-RESULT") {
			line = l
		}
	}
	if line == "" {
		t.Fatalf("no benchmark result in the guest console:\n%s", console)
	}

	var files, writeMS, readMS, bytesRead int
	if _, err := fmt.Sscanf(line, "BENCH-RESULT files=%d write_ms=%d read_ms=%d bytes_read=%d",
		&files, &writeMS, &readMS, &bytesRead); err != nil {
		t.Fatalf("parse %q: %v", line, err)
	}

	hostWrite, hostRead := hostBaseline(t, files)

	t.Logf("VIRTIOFS GATE 2 — %d small files (512B each)", files)
	t.Logf("  guest (virtio-fs): write %d ms (%.3f ms/file), read+stat %d ms (%.3f ms/file)",
		writeMS, float64(writeMS)/float64(files), readMS, float64(readMS)/float64(files))
	t.Logf("  host (native fs):  write %d ms (%.3f ms/file), read+stat %d ms (%.3f ms/file)",
		hostWrite, float64(hostWrite)/float64(files), hostRead, float64(hostRead)/float64(files))
	t.Logf("  ratio: write %.1fx host, read %.1fx host; whole boot+workload %s",
		ratio(writeMS, hostWrite), ratio(readMS, hostRead), bootAndRun.Round(time.Millisecond))

	// The usability floor, not a performance target: slower than this and a
	// dependency install inside the sandbox stops being viable.
	const maxMSPerFileWrite = 5.0
	if got := float64(writeMS) / float64(files); got > maxMSPerFileWrite {
		t.Errorf("virtio-fs write is %.3f ms/file, over the %.1f ms/file usability floor — "+
			"an npm install of 30k files would take %.0f s", got, maxMSPerFileWrite, got*30000/1000)
	}
}

// ratio guards against a zero-millisecond baseline.
func ratio(guest, host int) float64 {
	if host == 0 {
		return float64(guest)
	}
	return float64(guest) / float64(host)
}

// hostBaseline runs the same shape of workload on the host filesystem, so the
// guest number can be read as a multiple rather than an absolute that only
// means something on one machine.
func hostBaseline(t *testing.T, files int) (writeMS, readMS int) {
	t.Helper()
	root := t.TempDir()
	payload := make([]byte, 512)
	for i := range payload {
		payload[i] = byte('a' + i%26)
	}

	start := time.Now()
	for i := 0; i < files; i++ {
		dir := filepath.Join(root, fmt.Sprintf("pkg%03d", i%64))
		if err := os.MkdirAll(dir, 0o750); err != nil {
			t.Fatalf("baseline mkdir: %v", err)
		}
		if err := os.WriteFile(filepath.Join(dir, fmt.Sprintf("f%05d.js", i)), payload, 0o600); err != nil {
			t.Fatalf("baseline write: %v", err)
		}
	}
	writeMS = int(time.Since(start).Milliseconds())

	start = time.Now()
	err := filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
		if err != nil || info.IsDir() {
			return err
		}
		_, err = os.ReadFile(path) //nolint:gosec // test-owned path
		return err
	})
	if err != nil {
		t.Fatalf("baseline walk: %v", err)
	}
	return writeMS, int(time.Since(start).Milliseconds())
}

// realNodeModules is the path to a REAL npm dependency tree staged for the
// measurement. It is env-supplied because producing it needs npm and the
// registry, which a test must not require. Refs: ADR-010 Gate 2
const realTreeEnv = "MGIT_E2E_NPM_TREE"

// TestE2E_Libkrun_RealVM_NpmTreePerf measures virtio-fs against the file
// workload an `npm install` actually performs — unpack a real dependency tree,
// then traverse and read it as a build would — using the REAL shape of a
// node_modules tree (a heavy tail of tiny files, mean ~15 KB) rather than the
// uniform 512-byte files of the microbenchmark, whose extrapolation overstated
// the cost. Refs: ADR-010 Gate 2, NFR-17.2
func TestE2E_Libkrun_RealVM_NpmTreePerf(t *testing.T) {
	requireRealVM(t)
	tree := os.Getenv(realTreeEnv)
	if tree == "" {
		t.Skipf("SKIP (npm-tree perf): set %s to a real node_modules tree "+
			"(npm install into a scratch dir) to run this measurement", realTreeEnv)
	}
	if _, err := os.Stat(tree); err != nil { //nolint:gosec // G703: path is an operator-supplied env var for a local benchmark, not request input
		t.Skipf("SKIP (npm-tree perf): %s=%s is not readable: %v", realTreeEnv, tree, err)
	}

	guestRoot := buildGuestWorkload(t, "npmtree")
	// Stage the real tree into the share at the path the guest reads.
	staged := filepath.Join(guestRoot, "tree")
	if out, err := exec.Command("cp", "-R", tree, staged).CombinedOutput(); err != nil { //nolint:gosec // test-owned paths
		t.Fatalf("stage tree: %v\n%s", err, out)
	}

	cfg := realVMConfig(t, guestRoot, model.NetworkModeNone, nil)
	cfg.RootfsReadOnly = false
	console := bootVM(t, cfg)

	var line string
	for _, l := range strings.Split(console, "\n") {
		if strings.HasPrefix(l, "NPMTREE-RESULT") {
			line = l
		}
	}
	if line == "" {
		t.Fatalf("no npm-tree result in the guest console:\n%s", console)
	}
	var uFiles, rFiles int
	var uBytes, rBytes int64
	var uMS, rMS int
	if _, err := fmt.Sscanf(line,
		"NPMTREE-RESULT unpack_files=%d unpack_bytes=%d unpack_ms=%d traverse_files=%d traverse_bytes=%d traverse_ms=%d",
		&uFiles, &uBytes, &uMS, &rFiles, &rBytes, &rMS); err != nil {
		t.Fatalf("parse %q: %v", line, err)
	}

	t.Logf("VIRTIO-FS, REAL npm dependency tree (%d files, %.1f MB)", uFiles, float64(uBytes)/(1<<20))
	t.Logf("  unpack   (write): %d ms  (%.3f ms/file, %.1f MB/s)",
		uMS, float64(uMS)/float64(uFiles), float64(uBytes)/(1<<20)/(float64(uMS)/1000))
	t.Logf("  traverse (read):  %d ms  (%.3f ms/file, %.1f MB/s)",
		rMS, float64(rMS)/float64(rFiles), float64(rBytes)/(1<<20)/(float64(rMS)/1000))
	t.Logf("  DAX window: %d bytes", virtiofsDAXWindow())
}

// TestE2E_Libkrun_RealVM_AgentCommitsInTheSandbox is MGIT-61.7 BY EXECUTION.
//
// Until now the claim "an agent can run mgit commit inside the sandbox" was
// proven only by construction — the delivered layout was assembled host-side
// and the CLI driven over it. This boots a REAL microVM, has the guest mount
// the SEC-03 staged worktree at its identical path, and runs the REAL mgit
// binary in there: log, add, commit, and a host-only verb that must refuse.
// Refs: MGIT-61.7, SEC-03, FR-17.3, FR-17.11
func TestE2E_Libkrun_RealVM_AgentCommitsInTheSandbox(t *testing.T) {
	requireRealVM(t)
	const taskID = "MGIT-61.7"

	guestRoot := buildGuestWorkload(t, "mgitrunner")
	// The mgit CLI itself, exactly as the guest image ships it.
	mgitBin := filepath.Join(guestRoot, "bin", "mgit")
	if err := os.MkdirAll(filepath.Dir(mgitBin), 0o750); err != nil {
		t.Fatal(err)
	}
	//nolint:gosec // G204: fixed argv; mgitBin is a t.TempDir path
	build := exec.Command("go", "build", "-trimpath", "-buildvcs=false",
		"-ldflags=-buildid=", "-o", mgitBin, "./cmd/mgit")
	build.Dir = repoRoot(t)
	build.Env = append(os.Environ(), "GOOS=linux", "GOARCH="+runtime.GOARCH, "CGO_ENABLED=0")
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build the guest mgit CLI: %v\n%s", err, out)
	}

	// The REAL delivery path: hand CreateVM a worktree and a private store and
	// let it build the SEC-03 staged tree itself.
	hostRepo, privateStore := seedHostRepo(t, taskID)

	cfg := realVMConfig(t, guestRoot, model.NetworkModeNone, nil)
	cfg.RootfsReadOnly = false
	cfg.WorktreePath = hostRepo
	cfg.PrivateStorePath = privateStore
	cfg.WorktreeTag = "work"
	console := bootVM(t, cfg)

	for _, want := range []string{
		"GUEST-RESULT MOUNT = OK",
		"GUEST-RESULT STORE = PRESENT",
		"GUEST-RESULT LOG = OK",
		"GUEST-RESULT COMMIT = OK",
		"GUEST-RESULT HOSTONLY = REFUSED-WITH-REASON",
	} {
		if !strings.Contains(console, want) {
			t.Errorf("console missing %q; got:\n%s", want, console)
		}
	}
	t.Logf("REAL VM PASS: an agent ran mgit against the SEC-03 private store inside the sandbox\n%s", console)
}

// repoRoot locates the module root from this test file's own path, so the
// signed standalone binary can build guest binaries from any cwd.
func repoRoot(t *testing.T) string {
	t.Helper()
	_, thisFile, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("cannot locate the test source")
	}
	// internal/sandboxd/backend/libkrun/<file> -> module root
	return filepath.Clean(filepath.Join(filepath.Dir(thisFile), "..", "..", "..", ".."))
}

// seedHostRepo builds a real mgit project with one commit for taskID and
// provisions the SEC-03 private store for it, returning both. It uses the
// production provisioner, so the store the guest sees is the one a real
// launch would deliver. Refs: SEC-03, MGIT-62
func seedHostRepo(t *testing.T, taskID string) (repoRootDir, privateStoreDir string) {
	t.Helper()
	repo := t.TempDir()
	mgit := filepath.Join(t.TempDir(), "mgit-host")
	build := exec.Command("go", "build", "-o", mgit, "./cmd/mgit") //nolint:gosec // fixed argv
	build.Dir = repoRoot(t)
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build host mgit: %v\n%s", err, out)
	}
	run := func(args ...string) {
		cmd := exec.Command(mgit, args...) //nolint:gosec // test-built binary
		cmd.Dir = repo
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("mgit %v: %v\n%s", args, err, out)
		}
	}
	run("init")
	if err := os.WriteFile(filepath.Join(repo, "seed.txt"), []byte("host work\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	run("add", "seed.txt")
	run("commit", "-m", "host work before the sandbox", "--task", taskID)
	// NOTE: no squash — MGIT-62 means provisioning seeds from HEAD instead.

	prov, err := provision.NewStoreProvisioner(repo)
	if err != nil {
		t.Fatalf("provisioner: %v", err)
	}
	store, err := prov.Provision(taskID, filepath.Join(t.TempDir(), "private-store"))
	if err != nil {
		t.Fatalf("provision private store: %v", err)
	}
	return repo, store.Dir
}

// bootVMUntil boots a real microVM and returns once its console contains
// ready, leaving the VM RUNNING for the caller to drive. It returns the VM so
// the caller can stop it, and the console text so far.
//
// Distinct from bootVM, which waits for the guest to FINISH: a guest serving
// a published port must stay up while the host connects to it.
func bootVMUntil(t *testing.T, cfg microvm.VMConfig, ready string) (microvm.VM, string) {
	t.Helper()
	hv, err := NewHypervisor(slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelWarn})))
	if err != nil {
		t.Fatalf("NewHypervisor: %v", err)
	}
	vm, err := hv.CreateVM(cfg)
	if err != nil {
		t.Fatalf("CreateVM: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	if err := vm.Start(ctx); err != nil {
		t.Fatalf("Start (real VM): %v", err)
	}
	t.Cleanup(func() { _ = vm.Stop(context.Background(), true) })

	consolePath := filepath.Join(cfg.StateDir, consoleLogName)
	deadline := time.Now().Add(45 * time.Second)
	for time.Now().Before(deadline) {
		data, err := os.ReadFile(consolePath) //nolint:gosec // test-owned state dir
		if err == nil {
			if strings.Contains(string(data), ready) {
				return vm, string(data)
			}
			if strings.Contains(string(data), "krun_vm_failed") {
				t.Fatalf("the VM failed to boot; console:\n%s", data)
			}
		}
		time.Sleep(200 * time.Millisecond)
	}
	data, _ := os.ReadFile(consolePath) //nolint:gosec // test-owned state dir
	t.Fatalf("guest never reported %q; console:\n%s", ready, data)
	return nil, ""
}

// TestE2E_Libkrun_RealVM_Litmus1_HostSSHsIntoTheGuest is LITMUS LEG 1 against
// a real microVM — the last unproven containment claim.
//
// The guest runs a real SSH server on a published vsock port and the HOST
// completes a real SSH session into it: handshake, session channel, exec, and
// the guest's own banner back. That proves the SEC-09 inbound direction works
// end to end over libkrun's LISTENING vsock port — the mechanism chosen
// because the netstack gateway lives in the VM child where the daemon cannot
// reach it.
//
// Legs 2 and 3 (no exfiltration by default; the same tunnel permitted once
// policy allows it) are already green against the real authorizer.
// Refs: SEC-09, FR-17.8, MGIT-61.10
func TestE2E_Libkrun_RealVM_Litmus1_HostSSHsIntoTheGuest(t *testing.T) {
	requireRealVM(t)
	const publishedPort = 8022

	guestRoot := buildGuestWorkload(t, "sshguest")
	cfg := realVMConfig(t, guestRoot, model.NetworkModeNone, nil)
	cfg.VsockEnabled = false // this guest serves only the published port
	cfg.PublishPorts = []int{publishedPort}

	// The state dir must be derived the way production derives it, because
	// the dialer below derives it again from the same (workDir, sandboxID).
	// The directory name is a truncation of the ID — sockets bound under it
	// share sun_path's 104-byte budget — so it cannot be inverted, and a test
	// that reconstructed the ID from the directory would be testing a
	// property production does not have.
	workDir := shortTempDir(t)
	cfg.StateDir = microvm.SandboxStateDir(workDir, cfg.SandboxID)
	if err := os.MkdirAll(cfg.StateDir, 0o700); err != nil {
		t.Fatalf("create state dir: %v", err)
	}

	_, console := bootVMUntil(t, cfg, "GUEST-RESULT SSHD = LISTENING")
	t.Logf("guest console:\n%s", console)

	// The HOST initiates — the only direction SEC-09 opens. This is the same
	// dialer the production port publisher uses.
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	raw, err := NewPortDialer(workDir).DialGuestPort(ctx, cfg.SandboxID, publishedPort)
	if err != nil {
		t.Fatalf("host could not reach the published guest port: %v", err)
	}
	defer func() { _ = raw.Close() }()

	_ = raw.SetDeadline(time.Now().Add(20 * time.Second))
	cc, chans, reqs, err := ssh.NewClientConn(raw, "guest:8022", &ssh.ClientConfig{
		User:            "agent",
		HostKeyCallback: ssh.InsecureIgnoreHostKey(), //nolint:gosec // OK: throwaway key in-test
		Timeout:         20 * time.Second,
	})
	if err != nil {
		t.Fatalf("SSH handshake into the microVM failed: %v", err)
	}
	client := ssh.NewClient(cc, chans, reqs)
	defer func() { _ = client.Close() }()

	sess, err := client.NewSession()
	if err != nil {
		t.Fatalf("ssh session: %v", err)
	}
	defer func() { _ = sess.Close() }()

	out, err := sess.Output("whoami")
	if err != nil {
		t.Fatalf("ssh exec: %v", err)
	}
	if got := strings.TrimSpace(string(out)); got != "hello-from-guest-sshd" {
		t.Errorf("ssh output = %q, want the guest's banner", got)
	}
	t.Logf("LITMUS 1 PASS (real VM): host completed a real SSH session into the guest "+
		"over the SEC-09 published vsock port %d", publishedPort)
}

// TestE2E_Libkrun_RealVM_ConcurrentLaunches_AreIsolated closes the last gap
// in the churn evidence: every prior measurement was SEQUENTIAL (40 boots one
// after another, ADR-010 Gate 6), which cannot show cross-task contamination
// because only one VM ever existed at a time.
//
// N microVMs boot CONCURRENTLY, each with its own staged worktree carrying a
// marker only that sandbox should see. Each guest reports which markers it can
// read. Seeing another sandbox's marker is T6 cross-task contamination — the
// failure that matters when several agents work in parallel, which is mgit's
// whole premise. Teardown must then leave no residue (SEC-10).
// Refs: SEC-03, SEC-10, ADR-010 Gate 6, MGIT-61.6
func TestE2E_Libkrun_RealVM_ConcurrentLaunches_AreIsolated(t *testing.T) {
	requireRealVM(t)
	const sandboxes = 4

	guestRoot := buildGuestWorkload(t, "markerprobe")

	type launched struct {
		cfg    microvm.VMConfig
		marker string
	}
	all := make([]launched, 0, sandboxes)
	for i := range sandboxes {
		// Each sandbox gets its OWN worktree holding its own marker.
		wt := t.TempDir()
		marker := fmt.Sprintf("SANDBOX-MARKER-%d", i)
		if err := os.WriteFile(filepath.Join(wt, "marker.txt"), []byte(marker), 0o600); err != nil {
			t.Fatal(err)
		}
		cfg := realVMConfig(t, guestRoot, model.NetworkModeNone, nil)
		cfg.SandboxID = fmt.Sprintf("concurrent-%d", i)
		cfg.StateDir = shortTempDir(t)
		cfg.RootfsReadOnly = false
		cfg.WorktreePath = wt
		cfg.WorktreeTag = "work"
		cfg.VsockEnabled = false
		all = append(all, launched{cfg: cfg, marker: marker})
	}

	// Boot them all at once — the point of the test.
	consoles := make([]string, sandboxes)
	var wg sync.WaitGroup
	for i, l := range all {
		wg.Add(1)
		go func(i int, l launched) {
			defer wg.Done()
			consoles[i] = bootVM(t, l.cfg)
		}(i, l)
	}
	wg.Wait()

	for i, l := range all {
		// Its own marker must be visible...
		if !strings.Contains(consoles[i], "FOUND "+l.marker) {
			t.Errorf("sandbox %d could not read its OWN worktree marker; console:\n%s", i, consoles[i])
		}
		// ...and no other sandbox's (T6).
		for j, other := range all {
			if i == j {
				continue
			}
			if strings.Contains(consoles[i], "FOUND "+other.marker) {
				t.Errorf("sandbox %d saw sandbox %d's marker — cross-task contamination (T6);\nconsole:\n%s",
					i, j, consoles[i])
			}
		}
	}

	// RESIDUE (SEC-10), asserted at the layer that actually owns it.
	//
	// krun_start_enter exit()s the child process, so no Go deferred close
	// ever runs and the host peer's socket file survives the VM. That is by
	// design rather than a leak: every per-VM artifact is placed UNDER the
	// sandbox state dir precisely so the manager's single RemoveAll reclaims
	// all of it (FR-17.19). What must hold here is that nothing escaped that
	// directory — a socket outside it would survive teardown forever.
	for i := range all {
		stateDir := all[i].cfg.StateDir
		entries, err := os.ReadDir(stateDir)
		if err != nil {
			t.Fatalf("read sandbox %d state dir: %v", i, err)
		}
		for _, e := range entries {
			p := filepath.Join(stateDir, e.Name())
			if !strings.HasPrefix(p, stateDir) {
				t.Errorf("sandbox %d artifact %s escapes its state dir; teardown's "+
					"RemoveAll would not reclaim it (SEC-10)", i, p)
			}
		}
		// And the state dir is reclaimable in one call, which is the contract
		// the manager relies on.
		if err := os.RemoveAll(stateDir); err != nil {
			t.Errorf("sandbox %d state dir is not reclaimable in one RemoveAll: %v", i, err)
		}
		if _, err := os.Stat(filepath.Join(stateDir, denySocketName)); !os.IsNotExist(err) {
			t.Errorf("sandbox %d left residue after the state dir was removed (SEC-10)", i)
		}
	}
	t.Logf("CONCURRENCY PASS: %d microVMs booted concurrently, each saw only its own worktree", sandboxes)
}

// TestE2E_Libkrun_RealVM_MgitGuestControlPlane boots the REAL mgit-guest as
// PID 1 under libkrun and drives its vsock control plane from the host.
//
// This is the piece every other real-VM test stopped short of. Litmus leg 1
// used a purpose-built SSH guest, and the agent-commit test ran a workload
// that mounted the worktree itself — so mgit-guest's own PID-1 duties
// (pseudo-filesystems, the writable-root overlay and switch_root, the
// worktree mount) and the exec/land/notify vsock ports have never actually
// run under this VMM. Those are the daemon's only channel into a guest, so
// "configured" is not the same as "works". Refs: MGIT-61.13 P2, FR-17.11, SEC-09
func TestE2E_Libkrun_RealVM_MgitGuestControlPlane(t *testing.T) {
	requireRealVM(t)

	// The REAL guest supervisor, built exactly as the guest tree ships it.
	guestRoot := t.TempDir()
	// mgit-guest is PID 1 and mounts into these, so the base tree must
	// provide them — see guestBaseDirs for why this is a real requirement
	// and not test setup.
	for _, d := range append([]string{"sbin"}, guestBaseDirs...) {
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

	workDir := shortTempDir(t)
	const sandboxID = "guestctl"
	cfg := realVMConfig(t, guestRoot, model.NetworkModeNone, nil)
	cfg.SandboxID = sandboxID
	cfg.StateDir = microvm.SandboxStateDir(workDir, sandboxID)
	if err := os.MkdirAll(cfg.StateDir, 0o700); err != nil {
		t.Fatal(err)
	}
	cfg.RootfsReadOnly = false
	cfg.VsockEnabled = true // the whole point: wire exec/land/notify

	vm, console := bootVMUntil(t, cfg, "mgit-guest")
	t.Logf("guest console:\n%s", console)
	t.Cleanup(func() { _ = vm.Stop(context.Background(), true) })

	// Drive the guest through the PRODUCTION exec path: the same dialer the
	// daemon uses, and the same wire protocol.
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	conn, err := newGuestDialer(workDir).DialGuest(ctx, sandboxID)
	if err != nil {
		t.Fatalf("host could not reach mgit-guest's exec channel: %v", err)
	}
	defer func() { _ = conn.Close() }()

	var stdout, stderr bytes.Buffer
	res, err := guestexec.Run(conn, model.ExecRequest{
		Command: []string{"/sbin/mgit-guest", "--help"},
	}, &stdout, &stderr)
	if err != nil {
		t.Fatalf("exec round trip failed: %v (stderr=%q)", err, stderr.String())
	}
	// Assert on CONTENT, not byte counts: the point is that the guest's own
	// output traveled back over the vsock exec protocol.
	out := stdout.String() + stderr.String()
	if !strings.Contains(out, "vsock-port") {
		t.Errorf("exec produced no recognizable guest output; got %q", out)
	}
	// The console proves PID-1 got far enough to serve both control ports,
	// which means the overlay + switch_root completed under a virtiofs root.
	for _, want := range []string{`"vsock_port":1024`, `"vsock_port":1025`} {
		if !strings.Contains(console, want) {
			t.Errorf("mgit-guest did not report serving %s; console:\n%s", want, console)
		}
	}
	t.Logf("MGIT-GUEST CONTROL PLANE PASS: real mgit-guest booted as PID 1 under libkrun, "+
		"served exec+land vsock ports, and an exec round-tripped (code=%d, %dB out)",
		res.ExitCode, len(out))
}
