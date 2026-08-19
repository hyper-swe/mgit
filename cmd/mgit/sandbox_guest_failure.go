package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os/exec"
	"runtime"
	"strings"
	"time"

	"github.com/hyper-swe/mgit/internal/model"
)

// guestPhase names WHERE in a sandbox's life a failure was observed. It exists
// because the two shapes have different fixes and mgit was printing both at
// once: a launch that failed closed reported "guest never answered on its
// control channel" and then, three lines later, "the guest stopped answering
// mid-command" with a memory-cap advisory attached (MGIT-104).
//
// Exactly one phase is reported per failure, and only the phase the evidence
// supports. Refs: MGIT-104, R-H212
type guestPhase int

const (
	// phaseNeverStarted: the VM (or its guest agent) never answered on the
	// control channel, so no command ran inside it. The workload is not
	// implicated and neither, by construction, is in-guest memory exhaustion.
	phaseNeverStarted guestPhase = iota
	// phaseLostServing: the guest was reached and then stopped answering
	// mid-command — the shape real in-guest memory exhaustion takes, because
	// the kernel OOM killer takes the supervisor that would have reported it
	// (ADR-014). This is the phase the MGIT-95 cap advisory was built for.
	phaseLostServing
	// phaseDaemonStalled: mgit-sandboxd stopped emitting liveness beats on an
	// exec stream that was still open. NO conclusion about the guest follows
	// from this — the command may well still be running inside it — so this
	// phase exists precisely to keep the default from applying. Without it a
	// daemon-side stall lands in phaseLostServing and is reported as a guest
	// lost mid-command with a memory-cap advisory attached, which is the
	// MGIT-118 misdiagnosis rebuilt on a new cause. Refs: MGIT-133, MGIT-118
	phaseDaemonStalled
	// phaseVersionSkew: the CLI and the mgit-sandboxd it reached speak
	// different control-plane wire versions, so the two never transacted. This
	// is the strongest "not the guest" of the three: no command was sent, no
	// VM was consulted, and there is no sandbox in the story at all. It is a
	// separate phase for the same reason phaseDaemonStalled is — mgit 0.5.0
	// has no such phase, and its classifier duly reports a wire mismatch as a
	// guest lost mid-command with a memory-cap advisory (MGIT-136).
	// Refs: MGIT-136, MGIT-118
	phaseVersionSkew
)

// vmStartMarkers are console-log markers that appear ONLY when the VMM itself
// failed before the guest ran: the libkrun child logs krun_vm_failed on any
// pre-boot failure and the parent logs krun_vm_bootfail for a late handshake
// error, both of which mean krun_start_enter never handed control to a guest.
// Their presence is positive evidence of the never-started phase, independent
// of the wording of the surrounding error. Refs: MGIT-104, ADR-010
var vmStartMarkers = []string{"krun_vm_failed", "krun_vm_bootfail", "krun_start_enter"}

// startDetailMax bounds the console line quoted back to the caller; a guest
// can write an arbitrarily long line and a diagnosis must stay readable.
const startDetailMax = 300

// startDetailEllipsis marks a quoted console line that was truncated.
const startDetailEllipsis = " …"

// guestFailure is everything mgit CONCLUDED about a failed exec, from evidence
// it already holds at the moment it prints. Nothing here is inferred: an empty
// startDetail means no start failure was found, not that there was none.
// Refs: MGIT-104
type guestFailure struct {
	phase       guestPhase
	startDetail string           // the VMM start failure, verbatim, or "" when unidentified
	entitlement entitlementState // only ever consulted for a start failure
	daemonPath  string           // the daemon the entitlement verdict is about
}

// classifyGuestFailure decides which phase an exec failure was observed in.
//
// The evidence is the error mgit is about to print. Two independent signals
// put it in the never-started phase, and either alone is sufficient: the
// launch fail-closed sentinel (MGIT-92), and a VM-start marker in the guest
// console tail the same message carries. Everything else is a guest that was
// serving and was lost — the MGIT-95 case, which keeps its advisory.
//
// The sentinel is matched by TEXT as well as by errors.Is because an exec
// error crosses the daemon control protocol as a string (the result frame
// carries no error identity), so the in-process wrapping is gone by the time
// the CLI sees it. Refs: MGIT-104, MGIT-92, R-H212
func classifyGuestFailure(err error, ent entitlementState) guestFailure {
	if err == nil {
		return guestFailure{phase: phaseLostServing}
	}
	text := err.Error()
	// A version mismatch is settled BEFORE everything, including the daemon
	// stall. The two are ordered, not merely both first: a skew means the
	// peers never agreed to transact, so any other signal riding along in the
	// same message — a console tail, a wrapped transport error, even the stall
	// sentinel — is describing something that did not happen. Refs: MGIT-136
	if isVersionSkew(err) {
		return guestFailure{phase: phaseVersionSkew}
	}
	// A daemon that stopped answering is settled next and on its own: it is
	// the one remaining failure here that carries no evidence about the guest,
	// and every branch below would otherwise reach a guest-shaped conclusion
	// from it. Refs: MGIT-133
	if isDaemonStall(err) {
		return guestFailure{phase: phaseDaemonStalled}
	}
	detail := vmStartFailure(text)
	neverStarted := detail != "" ||
		errors.Is(err, model.ErrGuestNotServing) ||
		strings.Contains(text, model.ErrGuestNotServing.Error())
	if !neverStarted {
		return guestFailure{phase: phaseLostServing}
	}
	return guestFailure{phase: phaseNeverStarted, startDetail: detail, entitlement: ent}
}

// isDaemonStall reports whether an exec failure is the DAEMON's rather than
// the guest's.
//
// Two callers need this and they need it for different reasons: the classifier,
// so a host-side stall never reaches a guest-shaped conclusion, and the exec
// command, so it does not turn round and ask a daemon that just proved it
// cannot answer — that question hangs for the whole control-plane timeout
// before producing a diagnosis that never needed it.
//
// Matched by text as well as by errors.Is, for the same reason the
// ErrGuestNotServing check is: an exec failure can reach a caller as a string.
// Refs: MGIT-133
func isDaemonStall(err error) bool {
	return err != nil && (errors.Is(err, model.ErrSandboxDaemonUnresponsive) ||
		strings.Contains(err.Error(), model.ErrSandboxDaemonUnresponsive.Error()))
}

// isVersionSkew reports whether a failure is the two HOST binaries disagreeing
// about the wire, rather than anything that happened to a sandbox.
//
// It is matched by text as well as by errors.Is because a refused exec reaches
// the CLI through the daemon's result frame, which carries a string and no
// error identity — and a mismatch that arrives without identity would fall
// through to the guest default, which is the whole defect. A nil or unrelated
// error is NEVER a mismatch: silence and transport faults say nothing about a
// version, and guessing here would send a reader to upgrade binaries that are
// fine. Refs: MGIT-136
func isVersionSkew(err error) bool {
	return err != nil && (errors.Is(err, model.ErrSandboxVersionSkew) ||
		strings.Contains(err.Error(), model.ErrSandboxVersionSkew.Error()))
}

// vmStartFailure returns the guest console line identifying a VM-start
// failure, or "" when the evidence carries none. The guest agent logs JSON
// lines, so the error field is preferred over the whole record; a non-JSON
// line is quoted as written. Refs: MGIT-104
func vmStartFailure(text string) string {
	for _, line := range strings.Split(text, "\n") {
		if !containsAnyMarker(line) {
			continue
		}
		line = strings.TrimSpace(line)
		if inner := jsonLogError(line); inner != "" {
			line = inner
		}
		return truncateDetail(flatten(line))
	}
	return ""
}

// containsAnyMarker reports whether a console line carries a VM-start marker.
func containsAnyMarker(line string) bool {
	for _, marker := range vmStartMarkers {
		if strings.Contains(line, marker) {
			return true
		}
	}
	return false
}

// jsonLogError extracts the error field of a structured guest log line, or ""
// when the line is not such a record.
func jsonLogError(line string) string {
	var rec struct {
		Error string `json:"error"`
	}
	if err := json.Unmarshal([]byte(line), &rec); err != nil {
		return ""
	}
	return rec.Error
}

// flatten renders a multi-line value (a wrapped error carried inside one JSON
// log record) as a single line, so the quoted detail cannot break out of the
// sentence that introduces it.
func flatten(s string) string {
	return strings.Join(strings.Fields(strings.ReplaceAll(s, "\n", "; ")), " ")
}

// truncateDetail bounds a quoted console line to startDetailMax.
func truncateDetail(s string) string {
	if len(s) <= startDetailMax {
		return s
	}
	return s[:startDetailMax] + startDetailEllipsis
}

// writeGuestFailureAdvisory is what the exec paths call when the guest could
// not be reached. It reads the phase off the failure and prints the ONE
// diagnosis that matches it. Refs: MGIT-104, R-H212
func writeGuestFailureAdvisory(ctx context.Context, w io.Writer, info *model.SandboxInfo, err error) {
	f := classifyGuestFailure(err, entitlementUnknown)
	// The entitlement probe shells out to codesign, so it runs only where its
	// answer can matter: a VM that demonstrably failed to start.
	if f.phase == phaseNeverStarted && f.startDetail != "" {
		f.entitlement, f.daemonPath = probeHypervisorEntitlement(ctx)
	}
	writeGuestFailure(w, info, f)
}

// writeGuestFailure renders exactly one diagnosis for one phase.
// Refs: MGIT-104, MGIT-133
func writeGuestFailure(w io.Writer, info *model.SandboxInfo, f guestFailure) {
	switch f.phase {
	case phaseVersionSkew:
		writeVersionSkew(w)
	case phaseDaemonStalled:
		writeDaemonStall(w, info)
	case phaseLostServing:
		writeCapAdvisory(w, info, "the guest stopped answering mid-command")
	default:
		writeStartFailure(w, info, f)
	}
}

// writeVersionSkew reports a control-plane version mismatch.
//
// It names no sandbox and reads no state off one, deliberately: the CLI never
// spoke to the daemon, so any sandbox it might name is a guess, and naming one
// is what invites the reader back toward a guest-shaped explanation. The
// remedy itself was already printed — it travels inside the error this follows,
// single-sourced in controlproto.SkewMessage — so this adds only what that
// message cannot say: that NOTHING ran, and therefore that nothing about a
// guest or its memory cap is implicated. Refs: MGIT-136, MGIT-118
func writeVersionSkew(w io.Writer) {
	_, _ = fmt.Fprintf(w, "\nmgit: %s, so nothing ran — your command was never sent to a sandbox "+
		"and no guest was contacted.\n", model.ErrSandboxVersionSkew.Error())
	_, _ = fmt.Fprint(w, "This is a fault of the two host binaries, not of your workload and not of a "+
		"sandbox: do not resize anything and do not reshape the build. Upgrade both as shown above, "+
		"then confirm with `mgit --version` and `mgit-sandboxd --version`.\n")
}

// writeDaemonStall reports a daemon that stopped answering, and — as much as
// anything else here — reports what is NOT known.
//
// The command's fate is genuinely open: mgit-sandboxd relays a command's output
// only once it finishes, so a stall tells you the relay stopped, not that the
// work did. Saying so is the honest report and also the useful one, because the
// reader's next move (look at the daemon, expect the guest may still be busy)
// differs from every guest-failure path. It deliberately prints NO memory-cap
// advisory: the cap is not implicated by a host-side stall, and pointing at it
// is the MGIT-118 mistake. Refs: MGIT-133, MGIT-118
func writeDaemonStall(w io.Writer, info *model.SandboxInfo) {
	_, _ = fmt.Fprintf(w, "\nmgit: the sandbox daemon stopped answering%s — this is a failure of "+
		"mgit-sandboxd on THIS host, not of your command and not of the guest.\n", taskSuffix(info))
	_, _ = fmt.Fprintf(w, "Your command may still be running inside the guest: mgit-sandboxd relays a "+
		"command's output only once it finishes, so a stalled relay says nothing about the work.\n"+
		"Check the daemon with `mgit sandbox list` — if that hangs too, it is wedged and restarting "+
		"it (`mgit sandbox remove --task %s --force`, or killing mgit-sandboxd) is the way out.\n",
		taskName(info))
	_, _ = fmt.Fprint(w, "This sandbox's memory cap is not implicated: the daemon runs on the host, "+
		"outside it. Do not resize the sandbox or reshape the build for this.\n")
}

// writeStartFailure reports a guest that never ran.
//
// It deliberately does NOT print the memory-cap advisory. That advisory exists
// for a workload that died inside a running guest; appending it here pointed a
// reader at `--memory-mb` while the actual cause — an unsigned daemon that
// could not create a VM at all — sat three lines above in the console tail
// (MGIT-104). Where the cause is unidentified this says so and points at the
// evidence rather than nominating a suspect. Refs: MGIT-104, R-H212
func writeStartFailure(w io.Writer, info *model.SandboxInfo, f guestFailure) {
	_, _ = fmt.Fprintf(w, "\nmgit: the guest never started%s — no command ran inside it, "+
		"so this is a failure of the sandbox, not of your workload.\n", taskSuffix(info))
	if f.startDetail == "" {
		_, _ = fmt.Fprintf(w, "mgit could not identify the cause. The guest's own console output above is the "+
			"primary evidence; `mgit sandbox status %s` reports the backend and caps in force, and "+
			"docs/INSTALL-SANDBOX.md covers the daemon and guest-image prerequisites.\n", taskName(info))
		return
	}
	_, _ = fmt.Fprintf(w, "The VM itself failed to launch: %s\n", f.startDetail)
	if f.entitlement == entitlementMissing {
		// The probe resolved a real path; name it, so the fix is a command the
		// reader can paste rather than one that depends on PATH agreeing.
		if f.daemonPath == "" {
			f.daemonPath = daemonBinary
		}
		_, _ = fmt.Fprintf(w, "%s is not signed with the %s entitlement, which libkrun requires to create a VM "+
			"on macOS — this is the usual cause. Sign it and retry (from an mgit checkout):\n"+
			"  codesign --force --sign - --entitlements build/darwin/vz.entitlements %s\n"+
			"or install an already-signed build: brew install hyper-swe/tap/mgit (docs/INSTALL-SANDBOX.md).\n",
			f.daemonPath, hypervisorEntitlementKey, f.daemonPath)
	}
	_, _ = fmt.Fprint(w, "This sandbox's memory cap is not implicated: a guest that never started "+
		"cannot have exhausted its memory. Do not resize the sandbox or reshape the build for this.\n")
}

// taskSuffix renders " (task X)" when the sandbox is known, "" otherwise.
func taskSuffix(info *model.SandboxInfo) string {
	if info == nil || info.TaskID == "" {
		return ""
	}
	return fmt.Sprintf(" (task %s)", info.TaskID)
}

// taskName renders the task ID for use inside a suggested command, falling
// back to a placeholder rather than emitting a command that cannot be run.
func taskName(info *model.SandboxInfo) string {
	if info == nil || info.TaskID == "" {
		return "<task>"
	}
	return info.TaskID
}

// entitlementState is what mgit could determine about the sandbox daemon's
// macOS hypervisor entitlement. entitlementUnknown is a first-class answer:
// "cannot tell" must never be reported as "missing". Refs: MGIT-104
type entitlementState int

const (
	entitlementUnknown entitlementState = iota
	entitlementPresent
	entitlementMissing
)

// hypervisorEntitlementKey is the entitlement libkrun — the macOS GA backend
// (ADR-010) — requires to create a VM. It is a DIFFERENT key from vzf's
// com.apple.security.virtualization; checking the wrong one would clear a
// binary that cannot drive the backend this platform ships.
const hypervisorEntitlementKey = "com.apple.security.hypervisor"

// daemonBinary is the sandbox daemon program whose signing is in question.
const daemonBinary = "mgit-sandboxd"

// entitlementProbeTimeout bounds the codesign call; a diagnostic may never be
// the reason a failing command hangs.
const entitlementProbeTimeout = 3 * time.Second

// probeHypervisorEntitlement reports whether the mgit-sandboxd on this host
// carries the entitlement libkrun needs. Off darwin, or when the daemon or
// codesign cannot be found, the answer is entitlementUnknown and nothing is
// claimed. The check mirrors scripts/e2e/sandbox_posture.sh, which gates the
// live e2e on the same condition.
//
// The binary examined is the one locateSandboxd resolves — beside this mgit,
// then PATH — which is the daemon mgit itself activates, so the verdict is
// about the process that actually failed to start the VM rather than some
// other copy. Refs: MGIT-104, MGIT-64, MGIT-65
func probeHypervisorEntitlement(ctx context.Context) (entitlementState, string) {
	if runtime.GOOS != "darwin" {
		return entitlementUnknown, ""
	}
	path, err := locateSandboxd()
	if err != nil {
		return entitlementUnknown, ""
	}
	ctx, cancel := context.WithTimeout(ctx, entitlementProbeTimeout)
	defer cancel()
	// codesign writes the entitlements plist to stdout and its own complaints
	// (an unsigned binary) to stderr; both are evidence, so both are read.
	//nolint:gosec // fixed program; path is locateSandboxd's resolution of a fixed name
	out, err := exec.CommandContext(ctx, "codesign", "--display", "--entitlements", "-", path).CombinedOutput()
	return entitlementFromCodesign(string(out), err), path
}

// entitlementFromCodesign maps one codesign run to a verdict. A non-zero exit
// with output is a real answer — "code object is not signed at all" is exactly
// the case this diagnoses — while no output at all means codesign never ran,
// which is not evidence of anything. Refs: MGIT-104
func entitlementFromCodesign(output string, err error) entitlementState {
	if strings.Contains(output, hypervisorEntitlementKey) {
		return entitlementPresent
	}
	if err != nil && strings.TrimSpace(output) == "" {
		return entitlementUnknown
	}
	return entitlementMissing
}
