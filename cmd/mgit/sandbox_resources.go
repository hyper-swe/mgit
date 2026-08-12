package main

import (
	"fmt"
	"io"

	"github.com/spf13/cobra"

	"github.com/hyper-swe/mgit/internal/model"
)

// resourceFlags are the per-sandbox resource caps a caller may DECLARE at
// launch (R-H212). Zero means "use the host policy default" — the same
// contract model.SandboxLaunchOptions documents — and a value above the
// policy's per-sandbox maximum is REFUSED by the daemon naming the limit,
// never quietly reduced.
//
// The surface exists because its absence was itself a defect: with the fields
// present in the API and nothing able to set them, a workload that needed more
// memory than the default had no route except editing the operator's host
// policy — so a real build was reshaped to fit the sandbox instead.
// Refs: R-H212, NFR-17.5
type resourceFlags struct {
	CPUs        int
	MemoryMB    int
	DiskQuotaMB int
}

// bindResourceFlags registers the declarable resource caps on cmd. The
// spellings match the model/policy field names (`--memory-mb` <-> memory_mb
// <-> max_memory_mb) so a refusal message and the flag that fixes it read as
// the same vocabulary. Refs: R-H212
func bindResourceFlags(cmd *cobra.Command, r *resourceFlags) {
	cmd.Flags().IntVar(&r.CPUs, "cpus", 0, "vCPUs for this sandbox (0 = host policy default; over the policy maximum is refused, not clamped)")
	cmd.Flags().IntVar(&r.MemoryMB, "memory-mb", 0, "guest memory in MB (0 = host policy default; over the policy maximum is refused, not clamped)")
	cmd.Flags().IntVar(&r.DiskQuotaMB, "disk-quota-mb", 0, "guest disk quota in MB (0 = host policy default; over the policy maximum is refused, not clamped)")
}

// apply copies the declared caps into launch options. Refs: R-H212
func (r resourceFlags) apply(opts *model.SandboxLaunchOptions) {
	opts.CPUs, opts.MemoryMB, opts.DiskQuotaMB = r.CPUs, r.MemoryMB, r.DiskQuotaMB
}

// flagSuffix renders the declared caps back as CLI flags, so a "run this to
// retry" hint carries the sizing the caller asked for instead of silently
// dropping it. Empty when nothing was declared. Refs: R-H212
func (r resourceFlags) flagSuffix() string {
	suffix := ""
	for _, f := range []struct {
		name  string
		value int
	}{{"--cpus", r.CPUs}, {"--memory-mb", r.MemoryMB}, {"--disk-quota-mb", r.DiskQuotaMB}} {
		if f.value > 0 {
			suffix += fmt.Sprintf(" %s %d", f.name, f.value)
		}
	}
	return suffix
}

// capsLine renders a sandbox's EFFECTIVE resource caps for human output, or
// "" when none are known. `sandbox status` prints it so an agent can read its
// ceiling BEFORE concluding a build that died was the build's fault.
// Refs: R-H212
func capsLine(info *model.SandboxInfo) string {
	if info == nil || (info.MemoryMB == 0 && info.CPUs == 0 && info.DiskQuotaMB == 0) {
		return ""
	}
	line := "resources:"
	if info.CPUs > 0 {
		line += fmt.Sprintf(" %d vCPU", info.CPUs)
	}
	if info.MemoryMB > 0 {
		line += fmt.Sprintf(" %d MB memory", info.MemoryMB)
	}
	if info.DiskQuotaMB > 0 {
		line += fmt.Sprintf(" %d MB disk", info.DiskQuotaMB)
	}
	return line + "\n"
}

// signalExitCodes are the guest exit statuses a memory-exhaustion death
// presents as. They are NOT proof of one:
//
//   - 137 = 128+SIGKILL, how the guest kernel's OOM killer terminates a
//     process (the shell the host wraps a command in reports it);
//   - 134 = 128+SIGABRT, how a runtime that hits its OWN heap ceiling dies —
//     Node/V8 sizes old-gen from the memory it can see, so a small guest
//     produces "JavaScript heap out of memory" with no kernel event at all;
//   - -1 = the supervisor's report for a direct child killed by a signal
//     (Go's ProcessState.ExitCode has no signal encoding).
//
// A plain `kill -9` lands here too, which is why the advisory below is
// phrased as a possibility with a check the caller can run, never as a
// diagnosis. Refs: R-H212
var signalExitCodes = map[int]string{
	137: "SIGKILL — how the guest kernel's OOM killer terminates a process",
	134: "SIGABRT — how a runtime that exhausts its own heap aborts (Node/V8 sizes its heap from guest memory)",
	-1:  "a signal",
}

// writeMemoryAdvisory prints, at the point of failure, the ceiling the
// command ran under and what to do about it.
//
// This is the half of R-H212 that the resource flags alone do not fix. The
// customer incident did not begin with a missing flag; it began with an
// invisible ceiling: a build died with exit 134 and an opaque V8 message, and
// the agent — reasoning correctly from everything it could observe — reshaped
// the production bundler config to fit a limit it never knew existed. Naming
// the cap here, alongside the explicit instruction NOT to reshape the
// workload, is what makes that inference unnecessary.
//
// It is deliberately not a claim that the process ran out of memory (see
// signalExitCodes): mgit cannot see inside the guest's kernel from the host,
// so it reports what it knows for certain — the cap in force — and leaves the
// conclusion to the caller. Nothing is printed for an ordinary non-zero exit,
// or when the sandbox's cap is unknown. Refs: R-H212
func writeMemoryAdvisory(w io.Writer, info *model.SandboxInfo, exitCode int) {
	reason, suspicious := signalExitCodes[exitCode]
	if !suspicious {
		return
	}
	writeCapAdvisory(w, info, fmt.Sprintf("the command exited %d (%s)", exitCode, reason))
}

// writeCapAdvisory renders the shared body: the ceiling in force, the fact
// that it belongs to the sandbox rather than the project, and the command that
// raises it. Nothing is printed when the cap is unknown — an invented number
// would be worse than silence.
//
// Its callers are GATED: only a signal death inside a running guest, or a
// guest lost while it was serving, reaches here. A guest that never started
// gets writeStartFailure instead, because memory exhaustion is not a candidate
// for a guest that never ran (MGIT-104). Refs: R-H212, MGIT-104
func writeCapAdvisory(w io.Writer, info *model.SandboxInfo, lead string) {
	if info == nil || info.MemoryMB == 0 {
		return
	}
	_, _ = fmt.Fprintf(w, "\nmgit: %s.\n"+
		"This sandbox (task %s) is capped at %d MB of memory%s. Memory exhaustion inside the guest "+
		"presents exactly like this, and the cap is a property of THIS SANDBOX, not of your project.\n"+
		"If the workload needs more, declare it — do not reshape the build to fit the sandbox:\n"+
		"  mgit sandbox remove %s && mgit sandbox launch --task-id %s --worktree %s --memory-mb %d\n"+
		"(An over-large request is refused naming the host policy limit, so you will never silently get less.)\n",
		lead, info.TaskID, info.MemoryMB, cpuSuffix(info),
		info.TaskID, info.TaskID, info.WorktreePath, info.MemoryMB*2)
}

// cpuSuffix renders the vCPU part of the advisory when known.
func cpuSuffix(info *model.SandboxInfo) string {
	if info.CPUs <= 0 {
		return ""
	}
	return fmt.Sprintf(" and %d vCPU", info.CPUs)
}
