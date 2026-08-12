package main

import (
	"context"
	"fmt"
	"log/slog"
	"math"
	"time"

	"github.com/hyper-swe/mgit/internal/model"
	"github.com/hyper-swe/mgit/internal/store/policy"
)

// conservativeCeilingMB is the fail-closed aggregate memory ceiling used when
// the host's physical memory cannot be resolved. It is two default-sized
// (2048 MB) sandboxes: small enough that mgit never oversubscribes a host it
// could not measure, large enough that the daemon is still useful while the
// operator sizes it deliberately with --max-memory-mb.
//
// It exists because the alternative encodings are both worse. Zero would mean
// "no aggregate ceiling" to CeilingManager — the exact MGIT-98 fail-open. And
// refusing to start would leave an operator whose /proc/meminfo could not be
// read with no daemon at all, which is a bigger outage than a conservative cap.
// Refs: FR-17.26, MGIT-98
const conservativeCeilingMB = 4096

// bytesPerMiB converts the probe's bytes to the MB the ceiling is enforced in.
// (Like every other memory figure in mgit's sandbox surface, "MB" here means
// mebibytes — the unit the VMMs take.)
const bytesPerMiB = 1 << 20

// How the aggregate memory ceiling in force was arrived at. Recorded on the
// startup log so an operator can tell a policy-derived ceiling from an
// overridden one from a fallback without re-deriving it by hand.
const (
	// ceilingSourcePolicy: resolved from SandboxPolicy.MaxTotalMemoryPercent
	// against measured host physical memory. The default install.
	ceilingSourcePolicy = "policy"
	// ceilingSourceFlag: an explicit operator --max-memory-mb.
	ceilingSourceFlag = "flag"
	// ceilingSourceFallback: the host could not be measured; conservative
	// absolute in force.
	ceilingSourceFallback = "conservative-fallback"
	// ceilingSourceDisabled: host policy explicitly set the percentage to 0.
	ceilingSourceDisabled = "disabled-by-policy"
)

// fleetCeiling is the resolved FR-17.26 AGGREGATE (fleet-wide) admission bound.
//
// It is deliberately a different thing from the R-H212 per-sandbox maxima that
// MGIT-95 added: those bound what ONE launch may declare for itself and are
// enforced in the service, while this bounds what ALL sandboxes may hold at
// once and is enforced by the CeilingManager decorator. The two refusals stay
// distinguishable because they have different fixes — "ask for less" versus
// "free capacity, or this host is too small".
//
// What it counts is ADMITTED memory, not resident memory: libkrun and
// firecracker allocate guest pages lazily, so a 4096 MB guest that touches
// 300 MB still consumes its whole declared share of this ceiling. That is the
// conservative direction (mgit under-admits rather than over-commits), but it
// is not a measurement of host pressure and must not be described as one.
// Refs: FR-17.26, R-H212, MGIT-95, MGIT-98
type fleetCeiling struct {
	// maxTotalMemoryMB is the fleet ceiling handed to CeilingManager; 0
	// disables the memory dimension and is only ever reached deliberately.
	maxTotalMemoryMB int
	// defaultMemoryMB is what an undeclared launch is ACCOUNTED at, taken from
	// the host policy default so accounting matches what such a launch is
	// actually given.
	defaultMemoryMB int
	// source is one of the ceilingSource* constants.
	source string
	// hostMemoryMB is the measured host physical memory; 0 when unprobed.
	hostMemoryMB int
}

// resolveFleetCeiling determines the aggregate memory ceiling in force.
//
// Precedence: an explicit --max-memory-mb, else the host policy percentage
// resolved against measured host memory, else — if the host cannot be measured
// — a conservative absolute. It NEVER resolves to "unlimited" by accident;
// only an operator who sets max_total_memory_percent to 0 gets that, and it is
// logged as the deliberate choice it is. Refs: FR-17.26, SEC-09, MGIT-98
func resolveFleetCeiling(p model.SandboxPolicy, overrideMB int,
	probe func() (uint64, error), logger *slog.Logger) fleetCeiling {
	c := fleetCeiling{defaultMemoryMB: p.MemoryMB}
	switch {
	case overrideMB > 0:
		c.maxTotalMemoryMB, c.source = overrideMB, ceilingSourceFlag
	case p.MaxTotalMemoryPercent == 0:
		c.source = ceilingSourceDisabled
		logger.Warn("aggregate sandbox memory ceiling is DISABLED by host policy "+
			"(max_total_memory_percent=0); only the concurrent-sandbox cap bounds total fleet memory",
			"event", "fleet_memory_ceiling_disabled", "policy_field", "max_total_memory_percent")
	default:
		ceilingMB, hostMB, err := ceilingFromPolicy(p.MaxTotalMemoryPercent, probe)
		if err != nil {
			c.maxTotalMemoryMB, c.source = conservativeCeilingMB, ceilingSourceFallback
			logger.Warn("host physical memory could not be measured; failing closed to a conservative "+
				"aggregate sandbox memory ceiling rather than to no ceiling — size it deliberately "+
				"with mgit-sandboxd --max-memory-mb",
				"event", "fleet_memory_ceiling_fallback", "error", err.Error(),
				"ceiling_mb", conservativeCeilingMB)
			break
		}
		c.maxTotalMemoryMB, c.hostMemoryMB, c.source = ceilingMB, hostMB, ceilingSourcePolicy
	}
	logFleetCeiling(c, p, logger)
	return c
}

// ceilingFromPolicy turns a percent-of-host-memory policy into the MB the
// ceiling is enforced in. Both inputs are treated as hostile: an unreadable or
// implausible host size and an out-of-range percentage are errors, so the
// caller fails closed rather than deriving a nonsense ceiling. Refs: FR-17.26
func ceilingFromPolicy(percent int, probe func() (uint64, error)) (ceilingMB, hostMB int, err error) {
	if percent < 0 || percent > 100 {
		return 0, 0, fmt.Errorf("host policy max_total_memory_percent %d is outside 0-100", percent)
	}
	totalBytes, err := probe()
	if err != nil {
		return 0, 0, err
	}
	if totalBytes == 0 {
		return 0, 0, fmt.Errorf("host physical-memory probe reported 0 bytes")
	}
	hostMiB := totalBytes / bytesPerMiB
	if hostMiB > math.MaxInt32 {
		// 2 PiB. Not a host that exists; clamping keeps the int conversion
		// total rather than letting an absurd reading wrap into a negative
		// ceiling, which CeilingManager would read as disabled.
		hostMiB = math.MaxInt32
	}
	resolved := hostMiB * uint64(percent) / 100
	if resolved < 1 {
		// A truncated-to-zero ceiling would DISABLE the dimension downstream.
		// One MB refuses everything, which is the right direction for a host
		// this policy says has effectively no sandbox budget.
		resolved = 1
	}
	return int(resolved), int(hostMiB), nil
}

// logFleetCeiling states the ceiling actually in force at startup, so an
// operator never has to derive it from a percentage and a machine spec. The
// small-host warning is separate and deliberate: on a host where the ceiling
// lands below one default-sized sandbox, EVERY launch is refused, and that is
// far better learned at boot than from the first failed launch. mgit does not
// quietly raise the ceiling to make that go away — silently overriding the
// operator's stated percentage would oversubscribe a host they had sized on
// purpose. Refs: FR-17.26, MGIT-98
func logFleetCeiling(c fleetCeiling, p model.SandboxPolicy, logger *slog.Logger) {
	logger.Info("sandbox fleet memory ceiling resolved",
		"event", "fleet_memory_ceiling",
		"ceiling_mb", c.maxTotalMemoryMB,
		"source", c.source,
		"host_memory_mb", c.hostMemoryMB,
		"policy_percent", p.MaxTotalMemoryPercent,
		"accounted_default_mb", c.defaultMemoryMB,
		"counts", "admitted memory (declared per sandbox), not resident memory")

	if c.maxTotalMemoryMB > 0 && c.defaultMemoryMB > 0 && c.maxTotalMemoryMB < c.defaultMemoryMB {
		logger.Warn("this host is too small for the memory policy in force: the fleet ceiling is below "+
			"one default-sized sandbox, so EVERY launch will be refused until the policy or the host "+
			"changes; raise max_total_memory_percent, lower memory_mb, or pass --max-memory-mb",
			"event", "fleet_memory_ceiling_below_default",
			"ceiling_mb", c.maxTotalMemoryMB, "default_sandbox_mb", c.defaultMemoryMB,
			"policy_percent", p.MaxTotalMemoryPercent)
	}
}

// newPolicyStore opens the host policy store for daemon-start wiring, or
// returns nil when there is no host root to read (a greet-only daemon serves no
// launches, so it has no policy to enforce). Refs: FR-17.13
func newPolicyStore(hostRoot string, clock func() time.Time, logger *slog.Logger) *policy.Store {
	if hostRoot == "" {
		return nil
	}
	store, err := policy.NewStore(hostRoot, clock, slogPolicyRecorder{logger: logger})
	if err != nil {
		logger.Warn("host policy store could not be opened; sandbox ceilings fall back to safe defaults",
			"event", "policy_store_unavailable", "error", err.Error())
		return nil
	}
	return store
}

// loadDaemonPolicy reads the effective host policy for startup wiring.
//
// It never fails: an absent host root, an unreadable file, or a corrupt one all
// yield model.DefaultSandboxPolicy() with a warning. That is a deliberate
// asymmetry with the launch path, which fails closed per request — a daemon
// that refuses to BOOT because it could not parse policy.json leaves the
// operator with no sandbox service at all, while a daemon running the documented
// safe defaults leaves them with a bounded one. Launches still surface the
// policy error individually. Refs: FR-17.13, FR-17.26, MGIT-98
func loadDaemonPolicy(store *policy.Store, logger *slog.Logger) model.SandboxPolicy {
	if store == nil {
		return model.DefaultSandboxPolicy()
	}
	p, err := store.Load(context.Background())
	if err != nil {
		logger.Warn("host sandbox policy could not be read; resolving sandbox ceilings from the "+
			"safe defaults (launches will still report this error individually)",
			"event", "policy_load_failed", "error", err.Error())
		return model.DefaultSandboxPolicy()
	}
	return p
}
