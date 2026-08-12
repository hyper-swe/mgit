package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"log/slog"
	"math"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/hyper-swe/mgit/internal/model"
	"github.com/hyper-swe/mgit/internal/sandboxd/hostmem"
)

// probeOf returns a host-memory probe reporting mib mebibytes.
func probeOf(mib uint64) func() (uint64, error) {
	return func() (uint64, error) { return mib << 20, nil }
}

// failingProbe stands in for a platform whose host memory cannot be read.
func failingProbe() (uint64, error) {
	return 0, fmt.Errorf("no host physical-memory probe for plan9/riscv64")
}

// captureLogger returns a logger writing JSON records into buf.
func captureLogger(buf *bytes.Buffer) *slog.Logger {
	return slog.New(slog.NewJSONHandler(buf, nil))
}

// TestResolveFleetCeiling_FromPolicyPercent_NeedsNoOperatorFlag is the MGIT-98
// regression.
//
// Before it, cmd/mgit-sandboxd wired the FR-17.26 aggregate memory ceiling from
// --max-memory-mb, which defaults to 0, and 0 disables that dimension in
// CeilingManager. A default install therefore had NO fleet memory ceiling —
// only the concurrency cap. That was survivable while every sandbox took the
// 2048 MB policy default (8 x 2048 = ~16 GB), but MGIT-95 made per-sandbox
// memory declarable up to max_memory_mb (16384), so eight sandboxes could
// legally declare 128 GB with nothing to refuse them.
// Refs: FR-17.26, MGIT-95, MGIT-98
func TestResolveFleetCeiling_FromPolicyPercent_NeedsNoOperatorFlag(t *testing.T) {
	tests := []struct {
		name        string
		hostMiB     uint64
		percent     int
		wantCeiling int
		wantHostMB  int
	}{
		{name: "16gib_host_at_50_percent", hostMiB: 16384, percent: 50, wantCeiling: 8192, wantHostMB: 16384},
		{name: "64gib_host_at_50_percent", hostMiB: 65536, percent: 50, wantCeiling: 32768, wantHostMB: 65536},
		{name: "8gib_host_at_50_percent", hostMiB: 8192, percent: 50, wantCeiling: 4096, wantHostMB: 8192},
		{name: "operator_raised_to_75_percent", hostMiB: 16384, percent: 75, wantCeiling: 12288, wantHostMB: 16384},
		{name: "operator_lowered_to_10_percent", hostMiB: 16384, percent: 10, wantCeiling: 1638, wantHostMB: 16384},
		{name: "full_host_at_100_percent", hostMiB: 4096, percent: 100, wantCeiling: 4096, wantHostMB: 4096},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			p := model.DefaultSandboxPolicy()
			p.MaxTotalMemoryPercent = tt.percent

			got := resolveFleetCeiling(p, 0, probeOf(tt.hostMiB), testLogger())

			assert.Equal(t, tt.wantCeiling, got.maxTotalMemoryMB)
			assert.Equal(t, tt.wantHostMB, got.hostMemoryMB)
			assert.Equal(t, ceilingSourcePolicy, got.source)
			assert.Equal(t, p.MemoryMB, got.defaultMemoryMB,
				"accounting must use the policy default an undeclared launch actually gets")
		})
	}
}

// TestResolveFleetCeiling_FlagOverridesPolicy keeps --max-memory-mb meaningful:
// it becomes an explicit operator override rather than the only source. An
// operator inside a cgroup memory limit, where the host probe over-estimates
// the daemon's real budget, needs this. Refs: FR-17.26, MGIT-98
func TestResolveFleetCeiling_FlagOverridesPolicy(t *testing.T) {
	p := model.DefaultSandboxPolicy()

	got := resolveFleetCeiling(p, 3000, probeOf(16384), testLogger())

	assert.Equal(t, 3000, got.maxTotalMemoryMB, "the explicit flag wins over the policy percent")
	assert.Equal(t, ceilingSourceFlag, got.source)
	assert.Equal(t, p.MemoryMB, got.defaultMemoryMB)
}

// TestResolveFleetCeiling_ProbeFails_FailsClosedNotUnlimited pins the
// fail-closed direction. An unprobeable host must end up with a conservative
// absolute ceiling, never with the 0 that means "this dimension is disabled" —
// and it must not stop the daemon from starting either. Refs: FR-17.26, MGIT-98
func TestResolveFleetCeiling_ProbeFails_FailsClosedNotUnlimited(t *testing.T) {
	var logs bytes.Buffer
	p := model.DefaultSandboxPolicy()

	got := resolveFleetCeiling(p, 0, failingProbe, captureLogger(&logs))

	assert.Equal(t, conservativeCeilingMB, got.maxTotalMemoryMB)
	assert.NotZero(t, got.maxTotalMemoryMB,
		"zero means UNLIMITED to CeilingManager; an unprobeable host must never resolve to it")
	assert.Equal(t, ceilingSourceFallback, got.source)
	assert.Zero(t, got.hostMemoryMB, "an unprobed host memory size must not be reported as a number")

	out := logs.String()
	assert.Contains(t, out, "conservative", "the operator must be told the ceiling is a fallback")
	assert.Contains(t, out, "no host physical-memory probe", "and why the probe could not be trusted")
	assert.Contains(t, out, "--max-memory-mb", "and how to size it deliberately")
}

// TestResolveFleetCeiling_PolicyPercentZero_IsAnExplicitDisable distinguishes
// an operator who deliberately set max_total_memory_percent to 0 from the
// MGIT-98 accident of an off-by-default flag. Zero is a legal policy value
// (Validate allows 0-100), so it is honored — loudly. Refs: FR-17.26, MGIT-98
func TestResolveFleetCeiling_PolicyPercentZero_IsAnExplicitDisable(t *testing.T) {
	var logs bytes.Buffer
	p := model.DefaultSandboxPolicy()
	p.MaxTotalMemoryPercent = 0

	got := resolveFleetCeiling(p, 0, probeOf(16384), captureLogger(&logs))

	assert.Zero(t, got.maxTotalMemoryMB, "an explicit policy 0 disables the memory dimension")
	assert.Equal(t, ceilingSourceDisabled, got.source)
	assert.Contains(t, logs.String(), "max_total_memory_percent",
		"a disabled fleet memory ceiling must name the policy field that disabled it")
	assert.Contains(t, strings.ToUpper(logs.String()), "DISABLED")
}

// TestResolveFleetCeiling_SmallHost_ExplainsThatNoDefaultLaunchFits covers the
// modest-host case: 50% of physical memory can land BELOW the 2048 MB
// per-sandbox default, so every launch would be refused.
//
// The decision is deliberate: mgit keeps the honest ceiling rather than quietly
// raising it to fit one sandbox — silently overriding the operator's stated
// percentage is exactly the kind of clever auto-adjustment that leaves someone
// wondering why their host is oversubscribed. What mgit owes them instead is a
// clear explanation, at startup and again at the refusal.
// Refs: FR-17.26, MGIT-98
func TestResolveFleetCeiling_SmallHost_ExplainsThatNoDefaultLaunchFits(t *testing.T) {
	var logs bytes.Buffer
	p := model.DefaultSandboxPolicy() // MemoryMB 2048
	// A 2 GiB host at 50% leaves 1024 MB — below one default sandbox.
	got := resolveFleetCeiling(p, 0, probeOf(2048), captureLogger(&logs))

	assert.Equal(t, 1024, got.maxTotalMemoryMB, "the operator's stated percentage is honored, not quietly raised")
	assert.Equal(t, 2048, got.defaultMemoryMB)

	out := logs.String()
	assert.Contains(t, out, "too small",
		"startup must say plainly that this host cannot run even one default-sized sandbox")
	assert.Contains(t, out, "max_total_memory_percent", "and name the knob that would change that")
}

// TestResolveFleetCeiling_NeverRoundsDownToUnlimited guards the one arithmetic
// path that could re-create the MGIT-98 bug: a percent-of-host that truncates
// to 0 MB would hand CeilingManager the value that DISABLES the dimension.
// Refs: FR-17.26, MGIT-98
func TestResolveFleetCeiling_NeverRoundsDownToUnlimited(t *testing.T) {
	p := model.DefaultSandboxPolicy()
	p.MaxTotalMemoryPercent = 1

	// 1% of a 64 MiB host truncates to 0 MB.
	got := resolveFleetCeiling(p, 0, probeOf(64), testLogger())

	assert.Positive(t, got.maxTotalMemoryMB,
		"a rounding artifact must never disable the ceiling; 0 means unlimited downstream")
}

// TestResolveFleetCeiling_HostileProbeAndPolicy_FailClosed treats the probe and
// the percentage as hostile input: a zero-byte host, an absurd size, and an
// out-of-range percentage must all land on the conservative fallback rather
// than on a nonsense ceiling. Refs: FR-17.26, MGIT-98
func TestResolveFleetCeiling_HostileProbeAndPolicy_FailClosed(t *testing.T) {
	tests := []struct {
		name    string
		probe   func() (uint64, error)
		percent int
	}{
		{name: "probe_reports_zero_bytes", probe: func() (uint64, error) { return 0, nil }, percent: 50},
		{name: "negative_percent", probe: probeOf(16384), percent: -10},
		{name: "percent_over_100", probe: probeOf(16384), percent: 400},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			p := model.DefaultSandboxPolicy()
			p.MaxTotalMemoryPercent = tt.percent

			got := resolveFleetCeiling(p, 0, tt.probe, testLogger())

			assert.Equal(t, conservativeCeilingMB, got.maxTotalMemoryMB)
			assert.Equal(t, ceilingSourceFallback, got.source)
		})
	}
}

// TestResolveFleetCeiling_AbsurdHostSize_StaysAPositiveCeiling guards the int
// conversion: a probe reading larger than any real machine must not wrap into a
// negative ceiling, which CeilingManager would read as disabled.
// Refs: FR-17.26, MGIT-98
func TestResolveFleetCeiling_AbsurdHostSize_StaysAPositiveCeiling(t *testing.T) {
	p := model.DefaultSandboxPolicy()

	got := resolveFleetCeiling(p, 0, func() (uint64, error) { return math.MaxUint64, nil }, testLogger())

	assert.Positive(t, got.maxTotalMemoryMB, "an absurd reading must never wrap into a disabled ceiling")
	assert.Positive(t, got.hostMemoryMB)
}

// TestResolveFleetCeiling_RealHostProbe_ResolvesAgainstThisMachine runs the
// resolution against the REAL platform probe rather than a fake, so the wiring
// is verified end to end on whatever host the suite runs on. A ceiling that is
// merely plausible is the worst outcome here, so it is checked against the
// probe's own reading rather than against a hardcoded expectation.
// Refs: FR-17.26, MGIT-98
func TestResolveFleetCeiling_RealHostProbe_ResolvesAgainstThisMachine(t *testing.T) {
	total, err := hostmem.TotalBytes()
	if err != nil {
		t.Skipf("no host physical-memory probe on this platform: %v", err)
	}
	p := model.DefaultSandboxPolicy()

	got := resolveFleetCeiling(p, 0, hostmem.TotalBytes, testLogger())

	hostMB := int(total >> 20)
	assert.Equal(t, ceilingSourcePolicy, got.source)
	assert.Equal(t, hostMB, got.hostMemoryMB)
	assert.Equal(t, hostMB/2, got.maxTotalMemoryMB, "the default policy is 50% of host physical memory")
	t.Logf("resolved fleet ceiling on this host: %d MB of %d MB physical (%d%%), accounted default %d MB",
		got.maxTotalMemoryMB, got.hostMemoryMB, p.MaxTotalMemoryPercent, got.defaultMemoryMB)
}

// TestLoadDaemonPolicy_UnreadablePolicy_FallsBackWithoutBlockingStartup pins
// the second half of "fail closed, never to unlimited": a daemon that refuses
// to BOOT because it could not read a policy file leaves the operator worse off
// than one running on conservative defaults. Refs: FR-17.26, MGIT-98
func TestLoadDaemonPolicy_UnreadablePolicy_FallsBackWithoutBlockingStartup(t *testing.T) {
	clock := func() time.Time { return time.Unix(0, 0).UTC() }

	t.Run("no_host_root_uses_defaults", func(t *testing.T) {
		got := loadDaemonPolicy(newPolicyStore("", clock, testLogger()), testLogger())
		assert.Equal(t, model.DefaultSandboxPolicy().MaxTotalMemoryPercent, got.MaxTotalMemoryPercent)
		assert.Equal(t, model.DefaultSandboxPolicy().MemoryMB, got.MemoryMB)
	})

	t.Run("corrupt_policy_file_falls_back_loudly", func(t *testing.T) {
		var logs bytes.Buffer
		hostRoot := t.TempDir()
		require.NoError(t, os.WriteFile(filepath.Join(hostRoot, "policy.json"), []byte("{not json"), 0o600))

		got := loadDaemonPolicy(newPolicyStore(hostRoot, clock, testLogger()), captureLogger(&logs))

		assert.Equal(t, model.DefaultSandboxPolicy().MaxTotalMemoryPercent, got.MaxTotalMemoryPercent,
			"an unreadable policy must not become an absent ceiling")
		assert.Contains(t, logs.String(), "safe defaults")
	})

	t.Run("host_policy_is_honored_when_readable", func(t *testing.T) {
		hostRoot := t.TempDir()
		custom := model.DefaultSandboxPolicy()
		custom.MaxTotalMemoryPercent = 25
		custom.MemoryMB = 1024
		data, err := json.Marshal(custom)
		require.NoError(t, err)
		require.NoError(t, os.WriteFile(filepath.Join(hostRoot, "policy.json"), data, 0o600))

		got := loadDaemonPolicy(newPolicyStore(hostRoot, clock, testLogger()), testLogger())

		assert.Equal(t, 25, got.MaxTotalMemoryPercent)
		assert.Equal(t, 1024, got.MemoryMB)
	})
}

// TestRun_DefaultInstall_ResolvesALiveMemoryCeiling is the CLI-level proof, and
// the one this ticket most needs: unit tests over resolveFleetCeiling would
// still pass if main() never called it — which is precisely the shape of the
// MGIT-98 defect (a correct CeilingManager wired to a dead input).
//
// The daemon is started for real, with NO resource flags, and refused at
// backend selection (--backend container without the acknowledgment) so the
// test terminates deterministically on every platform. The ceiling is resolved
// before backend selection, so its startup record is on the log by then.
// Refs: FR-17.26, MGIT-98, MGIT-77, MGIT-83
func TestRun_DefaultInstall_ResolvesALiveMemoryCeiling(t *testing.T) {
	var out, logs bytes.Buffer

	code := run([]string{
		"--socket", filepath.Join(t.TempDir(), "sandboxd.sock"),
		"--backend", "container", // refused: no --acknowledge-reduced-isolation
	}, &out, &logs)

	require.Equal(t, 2, code, "the daemon stops at backend selection, after ceiling resolution")

	rec := findLogRecord(t, logs.String(), "fleet_memory_ceiling")
	require.NotNil(t, rec, "a default install must record the fleet memory ceiling it resolved:\n%s", logs.String())

	ceiling, ok := rec["ceiling_mb"].(float64)
	require.True(t, ok, "the startup record must state the resolved ceiling in MB: %v", rec)
	assert.Positive(t, ceiling,
		"a default install with no --max-memory-mb must still have a live fleet memory ceiling; "+
			"0 is the MGIT-98 defect (unlimited)")
	assert.Equal(t, float64(model.DefaultSandboxPolicy().MemoryMB), rec["accounted_default_mb"],
		"the accounted default must be the policy default an undeclared launch receives")
	assert.Equal(t, ceilingSourcePolicy, rec["source"],
		"on a probeable host the ceiling comes from policy, not from a flag or a fallback")

	t.Logf("daemon startup resolved: %v", rec)
}

// TestRun_ExplicitFlag_OverridesTheResolvedCeiling proves the operator override
// survives at the CLI boundary too. Refs: FR-17.26, MGIT-98
func TestRun_ExplicitFlag_OverridesTheResolvedCeiling(t *testing.T) {
	var out, logs bytes.Buffer

	code := run([]string{
		"--socket", filepath.Join(t.TempDir(), "sandboxd.sock"),
		"--backend", "container",
		"--max-memory-mb", "4242",
	}, &out, &logs)

	require.Equal(t, 2, code)
	rec := findLogRecord(t, logs.String(), "fleet_memory_ceiling")
	require.NotNil(t, rec)
	assert.Equal(t, float64(4242), rec["ceiling_mb"])
	assert.Equal(t, ceilingSourceFlag, rec["source"])
}

// findLogRecord returns the first JSON log record whose "event" field matches,
// or nil. The daemon logs JSON to its log sink, so records are parsed rather
// than substring-matched.
func findLogRecord(t *testing.T, logOutput, event string) map[string]any {
	t.Helper()
	for _, line := range strings.Split(strings.TrimSpace(logOutput), "\n") {
		if line == "" {
			continue
		}
		var rec map[string]any
		if err := json.Unmarshal([]byte(line), &rec); err != nil {
			continue
		}
		if rec["event"] == event {
			return rec
		}
	}
	return nil
}
