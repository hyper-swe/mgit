//go:build libkrun && cgo

package libkrun

import (
	"context"
	"fmt"
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
	if err := os.MkdirAll(filepath.Join(root, "sbin"), 0o755); err != nil {
		t.Fatalf("guest root: %v", err)
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
		if err == nil && strings.Contains(string(data), "GUEST: done") {
			return string(data)
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
// TSI default. Refs: FR-17.7, SEC-04, ADR-010
func TestE2E_Libkrun_RealVM_NoneMode_NoEgress(t *testing.T) {
	requireRealVM(t)
	guestRoot := buildGuest(t)

	cfg := realVMConfig(t, guestRoot, model.NetworkModeNone, nil)
	console := bootVM(t, cfg)

	if !strings.Contains(console, "GUEST-RESULT OFF_ALLOWLIST = DENIED") {
		t.Fatalf("guest reached the network in none mode — TSI leak or missing NIC (ADR-010); console:\n%s", console)
	}
	t.Logf("REAL VM PASS: none mode denied guest egress\n%s", console)
}

// TestE2E_Libkrun_RealVM_Allowlist_DefaultDeny proves the same claim through
// the REAL egress authorizer: allowlist mode runs the netstack gateway in the
// VM's own child process, and a destination the policy does not name is reset
// rather than proxied. This is litmus assertion 2 (no reverse tunnel / no
// exfiltration) against a real guest. Refs: SEC-04, FR-17.8
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
	t.Logf("REAL VM PASS: allowlist mode default-denied an off-list destination\n%s", console)
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
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatalf("baseline mkdir: %v", err)
		}
		if err := os.WriteFile(filepath.Join(dir, fmt.Sprintf("f%05d.js", i)), payload, 0o644); err != nil {
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
	if _, err := os.Stat(tree); err != nil {
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
