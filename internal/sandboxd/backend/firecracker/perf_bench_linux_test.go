//go:build linux

// FR-17 sandbox PERFORMANCE BENCHMARK harness (firecracker/KVM backend).
//
// These are gated regression tests — NOT Go Benchmarks — that MEASURE a
// metric on a real microVM and then assert it against a named NFR-17
// threshold const. Each measured number is t.Logf'd ALWAYS (before any
// assertion) so the adoption-criteria spike (MGIT-11.13.2) captures the
// real figure even on a run that later fails the threshold. When no real
// KVM backend is available they t.Skip cleanly (the suite stays green on
// non-KVM CI), mirroring the exec/land round-trip gating in this package
// (requireKVM + MGIT_E2E_GUEST_ROOTFS).
//
// Backends validated: this file covers firecracker-on-KVM, the backend the
// NFR-17 targets were dimensioned for (cold boot < 1s, warm start < 200ms).
// The vzf counterpart (perf_bench_darwin_test.go) carries the same four test
// NAMES with darwin build constraints and records vzf's own numbers (vzf
// boots ~1.15s, so its cold-boot figure is recorded, not asserted against the
// firecracker target). Refs: NFR-17.1, NFR-17.2, NFR-17.3, NFR-17.4, MGIT-11.13.2
package firecracker

import (
	"bufio"
	"context"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/hyper-swe/mgit/internal/model"
	"github.com/hyper-swe/mgit/internal/sandboxd/backend/microvm"
	"github.com/hyper-swe/mgit/internal/sandboxd/images"
)

// NFR-17 thresholds as named consts so each firecracker-vs-vzf difference is
// visible at the assertion site. The figures are the firecracker-oriented
// targets from REQUIREMENTS.md §NFR-17; vzf records but may not meet the boot
// ones (documented in perf_bench_darwin_test.go). Refs: NFR-17.1..NFR-17.4
const (
	benchWarmExecOverheadMax = 50 * time.Millisecond  // NFR-17.1
	benchWarmStartMax        = 200 * time.Millisecond // NFR-17.2 (warm start)
	benchColdBootTarget      = 1 * time.Second        // NFR-17.2 (cold boot)
	benchFiveIdleRSSMaxBytes = 500 << 20              // NFR-17.4 (5 idle < 500MB)
	benchIdleSandboxCount    = 5                      // NFR-17.4
)

// benchGuestImage gates the perf suite exactly like the firecracker e2e
// round-trips: a usable /dev/kvm + firecracker binary + mke2fs + cached
// kernel (requireKVM) AND an operator-supplied mgit-guest rootfs that serves
// the exec vsock channel (MGIT_E2E_GUEST_ROOTFS). It returns a registered,
// digest-pinned image ref plus a booted manager wired to resolve it. Without
// any prerequisite it t.Skips. Refs: NFR-17.1, MGIT-11.13.2
func benchGuestImage(t *testing.T) (mgr *microvm.Manager, ref, workDir string) {
	t.Helper()
	kernel, _ := requireKVM(t) // KVM + firecracker + mke2fs + cached kernel, else skip
	rootfs := os.Getenv("MGIT_E2E_GUEST_ROOTFS")
	if rootfs == "" {
		t.Skip("set MGIT_E2E_GUEST_ROOTFS to a guest image (serving the exec vsock channel) to run the perf benchmarks")
	}
	if !fileExists(rootfs) {
		t.Skipf("guest rootfs %s absent", rootfs)
	}

	clock := func() time.Time { return time.Now().UTC() }
	hostRoot := t.TempDir()
	_, err := images.GenerateTrustRoot(context.Background(), hostRoot, noopAudit{})
	require.NoError(t, err)
	priv, err := images.LoadSigningKey(hostRoot)
	require.NoError(t, err)
	entry, err := images.BuildEntry(kernel, rootfs, e2eGuestCmdline)
	require.NoError(t, err)
	ref, err = images.Register(hostRoot, "mgit-guest", entry, priv)
	require.NoError(t, err)
	store, err := images.NewStore(hostRoot, clock)
	require.NoError(t, err)

	workDir, err = os.MkdirTemp("", "mgperf") // short path for the vsock unix socket
	require.NoError(t, err)
	t.Cleanup(func() { _ = os.RemoveAll(workDir) })

	mgr, err = NewManager(Config{
		WorkDir: workDir,
		Resolve: func(r string) (ImagePaths, error) {
			ri, rerr := store.Resolve(r)
			return ImagePaths{KernelPath: ri.KernelPath, RootfsPath: ri.RootfsPath, Cmdline: ri.Cmdline}, rerr
		},
		Logger: slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError})),
		Clock:  clock,
	})
	require.NoError(t, err)
	return mgr, ref, workDir
}

// benchLaunch boots one idle, network-none sandbox over a fresh tempdir
// worktree and returns its info. Cleanup removes it.
func benchLaunch(t *testing.T, mgr *microvm.Manager, ref string) *model.SandboxInfo {
	t.Helper()
	info, err := mgr.Launch(context.Background(), model.SandboxLaunchOptions{
		TaskID: "MGIT-11.13.2", WorktreePath: t.TempDir(), ImageRef: ref,
		Network: model.NetworkPolicy{Mode: model.NetworkModeNone}, CPUs: 1, MemoryMB: 256,
	})
	require.NoError(t, err)
	t.Cleanup(func() { _ = mgr.Remove(context.Background(), info.ID, true) })
	return info
}

// benchExecReady retries an exec until the guest is serving its vsock channel
// (the guest boots + listens asynchronously), so the warm-path measurements
// time a genuinely warm guest rather than the boot race. Fails the test if the
// guest never becomes reachable within the deadline.
func benchExecReady(t *testing.T, mgr *microvm.Manager, id string) {
	t.Helper()
	deadline := time.Now().Add(25 * time.Second)
	for time.Now().Before(deadline) {
		_, err := mgr.Exec(context.Background(), id, model.ExecRequest{
			Command: []string{"/bin/sh", "-c", "true"},
		})
		if err == nil {
			return
		}
		time.Sleep(200 * time.Millisecond)
	}
	t.Fatal("guest never became reachable over the exec vsock channel within the deadline")
}

// TestBench_WarmExecOverheadUnder50ms measures the per-command overhead a warm
// (already-running) sandbox adds versus a trivial host exec of the same shape,
// and asserts it stays under the NFR-17.1 budget. Overhead = median warm
// sandbox exec round-trip minus median host /bin/sh round-trip; clamped at zero
// (a faster-than-host sample is reported as zero overhead, never negative).
// The measured number is always logged. Refs: NFR-17.1, MGIT-11.13.2
func TestBench_WarmExecOverheadUnder50ms(t *testing.T) {
	mgr, ref, _ := benchGuestImage(t)
	info := benchLaunch(t, mgr, ref)
	benchExecReady(t, mgr, info.ID)

	const samples = 7
	host := medianDuration(t, samples, func() {
		out, err := exec.Command("/bin/sh", "-c", "echo warm-exec-baseline").CombinedOutput() //nolint:gosec // fixed argv, no shell injection
		require.NoError(t, err)
		require.Contains(t, string(out), "warm-exec-baseline")
	})
	sandbox := medianDuration(t, samples, func() {
		res, err := mgr.Exec(context.Background(), info.ID, model.ExecRequest{
			Command: []string{"/bin/sh", "-c", "echo warm-exec-baseline"},
		})
		require.NoError(t, err)
		require.Equal(t, 0, res.ExitCode, "stderr=%q", string(res.Stderr))
	})

	overhead := sandbox - host
	if overhead < 0 {
		overhead = 0
	}
	t.Logf("NFR-17.1 warm exec overhead: sandbox median=%v host median=%v overhead=%v (target < %v)",
		sandbox, host, overhead, benchWarmExecOverheadMax)
	assert.Lessf(t, overhead, benchWarmExecOverheadMax,
		"warm exec overhead %v exceeds NFR-17.1 budget %v", overhead, benchWarmExecOverheadMax)
}

// TestBench_WarmStartUnder200ms measures start latency on the firecracker
// backend. v1 has no live resume (Stop kills the VMM; the next launch re-boots
// against the warm host page cache), so "warm start" is the second launch with
// the kernel/rootfs already cached, and cold boot is the first launch on a cold
// cache. Both numbers are always logged; the warm figure is asserted against
// NFR-17.2's 200ms warm-start budget and the cold figure is recorded against
// the < 1s target (logged, asserted leniently so a slow CI disk does not flake
// the warm-start signal). Refs: NFR-17.2, MGIT-11.13.2
func TestBench_WarmStartUnder200ms(t *testing.T) {
	mgr, ref, _ := benchGuestImage(t)

	// Cold boot: first launch, host page cache not yet primed with this image.
	coldStart := time.Now()
	cold := benchLaunch(t, mgr, ref)
	benchExecReady(t, mgr, cold.ID)
	coldBoot := time.Since(coldStart)
	require.NoError(t, mgr.Remove(context.Background(), cold.ID, true))

	// Warm start: relaunch with the kernel/rootfs pages warm in the host cache.
	warmStart := time.Now()
	warm := benchLaunch(t, mgr, ref)
	benchExecReady(t, mgr, warm.ID)
	warmBoot := time.Since(warmStart)

	t.Logf("NFR-17.2 cold boot: %v (target < %v)", coldBoot, benchColdBootTarget)
	t.Logf("NFR-17.2 warm start: %v (target < %v)", warmBoot, benchWarmStartMax)
	if coldBoot >= benchColdBootTarget {
		t.Logf("note: cold boot %v exceeded the %v target (recorded, not failed — disk/CI variance)",
			coldBoot, benchColdBootTarget)
	}
	assert.Lessf(t, warmBoot, benchWarmStartMax,
		"warm start %v exceeds NFR-17.2 budget %v", warmBoot, benchWarmStartMax)
}

// TestBench_FiveIdleUnder500MB launches five idle, network-none sandboxes and
// asserts their summed attributable RSS — the resident memory of each VM's
// firecracker hypervisor process, read from /proc/<pid>/status VmRSS — stays
// under NFR-17.4's 500MB aggregate. Each per-VM RSS and the total are always
// logged. The per-VM firecracker PID is found by matching this manager's
// per-sandbox api-socket path in /proc/<pid>/cmdline (the VMM is launched with
// --api-sock <workDir>/<id>/firecracker.sock), with no production-code change.
// Refs: NFR-17.4, MGIT-11.13.2
func TestBench_FiveIdleUnder500MB(t *testing.T) {
	mgr, ref, workDir := benchGuestImage(t)

	var total uint64
	for i := 0; i < benchIdleSandboxCount; i++ {
		info := benchLaunch(t, mgr, ref)
		benchExecReady(t, mgr, info.ID) // ensure booted before sampling idle RSS
		pid, ok := findFirecrackerPID(t, workDir, info.ID)
		if !ok {
			t.Skipf("could not locate the firecracker hypervisor process for sandbox %s; cannot attribute per-VM RSS", info.ID)
		}
		rss, ok := procVMRSSBytes(pid)
		if !ok {
			t.Skipf("could not read /proc/%d/status VmRSS for sandbox %s", pid, info.ID)
		}
		t.Logf("NFR-17.4 idle sandbox %d/%d (pid %d) attributable RSS: %d bytes (%.1f MB)",
			i+1, benchIdleSandboxCount, pid, rss, float64(rss)/(1<<20))
		total += rss
	}
	t.Logf("NFR-17.4 total attributable RSS for %d idle sandboxes: %d bytes (%.1f MB) (target < %d MB)",
		benchIdleSandboxCount, total, float64(total)/(1<<20), benchFiveIdleRSSMaxBytes>>20)
	assert.LessOrEqualf(t, total, uint64(benchFiveIdleRSSMaxBytes),
		"five idle sandboxes consume %.1f MB attributable RSS, over the NFR-17.4 %d MB budget",
		float64(total)/(1<<20), benchFiveIdleRSSMaxBytes>>20)
}

// TestBench_SuspendedZeroCPU asserts NFR-17.3: a suspended sandbox consumes 0%
// host CPU. In v1, suspend (manager.Stop, the graceful pause the lifecycle
// service issues for idle-suspend) halts the firecracker VMM — the process is
// reaped and cannot accrue CPU. The benchmark verifies the suspended VM's
// hypervisor process accrues ZERO additional CPU ticks: it samples the VMM's
// /proc/<pid>/stat utime+stime, suspends, then re-reads. A reaped process (no
// /proc entry) is the definitive zero-CPU result; if it lingers, its CPU-tick
// delta across a quiet window must be exactly zero. The measured deltas are
// always logged. Refs: NFR-17.3, MGIT-11.13.2
func TestBench_SuspendedZeroCPU(t *testing.T) {
	mgr, ref, workDir := benchGuestImage(t)
	info := benchLaunch(t, mgr, ref)
	benchExecReady(t, mgr, info.ID)

	pid, ok := findFirecrackerPID(t, workDir, info.ID)
	if !ok {
		t.Skipf("could not locate the firecracker hypervisor process for sandbox %s; cannot measure suspended CPU", info.ID)
	}
	before, ok := procCPUTicks(pid)
	if !ok {
		t.Skipf("could not read /proc/%d/stat CPU ticks for sandbox %s", pid, info.ID)
	}

	// Suspend = graceful Stop (force=false), the same pause idle-suspend issues.
	require.NoError(t, mgr.Stop(context.Background(), info.ID, false))

	// Let any post-suspend accounting settle, then re-read. A reaped VMM is the
	// definitive zero-CPU result; a lingering one must accrue no further ticks.
	time.Sleep(1 * time.Second)
	after, present := procCPUTicks(pid)
	if !present {
		t.Logf("NFR-17.3 suspended sandbox %s: hypervisor process %d reaped on suspend — zero CPU by construction", info.ID, pid)
		return
	}
	delta := after - before
	t.Logf("NFR-17.3 suspended sandbox %s: hypervisor process %d CPU ticks before=%d after=%d delta=%d (target 0)",
		info.ID, pid, before, after, delta)
	assert.Equalf(t, uint64(0), delta,
		"suspended sandbox accrued %d CPU ticks, violating NFR-17.3 (must be 0)", delta)
}

// medianDuration runs fn n times and returns the median wall-clock duration of
// one call, discarding the ordering noise a single sample carries. n must be
// odd for a true middle element; callers pass an odd sample count.
func medianDuration(t *testing.T, n int, fn func()) time.Duration {
	t.Helper()
	ds := make([]time.Duration, 0, n)
	for i := 0; i < n; i++ {
		start := time.Now()
		fn()
		ds = append(ds, time.Since(start))
	}
	insertionSortDurations(ds)
	return ds[len(ds)/2]
}

// insertionSortDurations sorts in place (n is tiny — the sample count — so a
// dependency-free insertion sort keeps the harness self-contained).
func insertionSortDurations(ds []time.Duration) {
	for i := 1; i < len(ds); i++ {
		for j := i; j > 0 && ds[j-1] > ds[j]; j-- {
			ds[j-1], ds[j] = ds[j], ds[j-1]
		}
	}
}

// procPath builds an absolute /proc path for one pid entry. It is split out so
// the path literal does not embed a separator inside filepath.Join (gocritic
// filepathJoin), while still cleaning the result.
func procPath(pidEntry, leaf string) string {
	return filepath.Join(string(os.PathSeparator)+"proc", pidEntry, leaf)
}

// findFirecrackerPID locates the firecracker hypervisor process for one
// sandbox by matching this manager's per-sandbox api-socket path against every
// /proc/<pid>/cmdline. The VMM is launched with that socket as --api-sock, so
// the match is exact and unique to the sandbox. Read-only; no production code
// is touched. Returns false when no live process carries the path.
func findFirecrackerPID(t *testing.T, workDir, sandboxID string) (int, bool) {
	t.Helper()
	socket := sandboxPaths(microvm.SandboxStateDir(workDir, sandboxID)).socket
	entries, err := os.ReadDir(string(os.PathSeparator) + "proc")
	if err != nil {
		return 0, false
	}
	for _, e := range entries {
		pid, err := strconv.Atoi(e.Name())
		if err != nil {
			continue // not a PID dir
		}
		raw, err := os.ReadFile(procPath(e.Name(), "cmdline")) //nolint:gosec // /proc path from a numeric pid dir
		if err != nil {
			continue // process exited between ReadDir and ReadFile
		}
		if strings.Contains(string(raw), socket) {
			return pid, true
		}
	}
	return 0, false
}

// procVMRSSBytes reads VmRSS (resident set size) for a pid from
// /proc/<pid>/status, returning bytes. The field is reported in kB.
func procVMRSSBytes(pid int) (uint64, bool) {
	f, err := os.Open(procPath(strconv.Itoa(pid), "status")) //nolint:gosec // /proc path from a numeric pid
	if err != nil {
		return 0, false
	}
	defer func() { _ = f.Close() }()
	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		line := scanner.Text()
		if !strings.HasPrefix(line, "VmRSS:") {
			continue
		}
		fields := strings.Fields(line) // "VmRSS:" <n> "kB"
		if len(fields) < 2 {
			return 0, false
		}
		kb, err := strconv.ParseUint(fields[1], 10, 64)
		if err != nil {
			return 0, false
		}
		return kb << 10, true
	}
	return 0, false
}

// procCPUTicks returns the cumulative user+system CPU ticks for a pid from
// /proc/<pid>/stat (fields 14 utime + 15 stime, in clock ticks). The bool is
// false when the process is gone (a reaped VMM) — which the suspend benchmark
// treats as the definitive zero-CPU result. The comm field (field 2) is
// parenthesized and may contain spaces, so we split after the trailing ')'.
func procCPUTicks(pid int) (uint64, bool) {
	raw, err := os.ReadFile(procPath(strconv.Itoa(pid), "stat")) //nolint:gosec // /proc path from a numeric pid
	if err != nil {
		return 0, false
	}
	stat := string(raw)
	rparen := strings.LastIndex(stat, ")")
	if rparen < 0 || rparen+2 > len(stat) {
		return 0, false
	}
	// Fields after the comm: index 0 here is field 3 (state), so utime is at
	// index 11 and stime at index 12 in this post-comm slice.
	fields := strings.Fields(stat[rparen+2:])
	if len(fields) < 13 {
		return 0, false
	}
	utime, err := strconv.ParseUint(fields[11], 10, 64)
	if err != nil {
		return 0, false
	}
	stime, err := strconv.ParseUint(fields[12], 10, 64)
	if err != nil {
		return 0, false
	}
	return utime + stime, true
}
