package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"

	"github.com/hyper-swe/mgit/internal/model"
)

// guestPhase names WHERE in a sandbox's life a failure was observed. It exists
// because the shapes have different fixes and mgit was printing two at once: a
// launch that failed closed reported "guest never answered on its control
// channel" and then, three lines later, "the guest stopped answering
// mid-command" with a memory-cap advisory attached (MGIT-104).
//
// Exactly one phase is reported per failure, and only the phase the evidence
// SUPPORTS — every phase below requires positive evidence of its own, and a
// failure that produces none is phaseUnidentified rather than a guess.
// Refs: MGIT-118, MGIT-104, R-H212
type guestPhase int

const (
	// phaseUnidentified: mgit could not place the failure, and says so. This is
	// the default, and it is deliberately the ZERO VALUE so that a phase nobody
	// assigned cannot diagnose anything either.
	//
	// It is the default because the alternative was tried and failed four
	// times. phaseLostServing held this position, and four distinct causes had
	// to be carved out of it BY NAME after each was reported to a user as
	// in-guest memory exhaustion: a VM that never booted (MGIT-104), a fleet
	// ceiling refusing admission (MGIT-118), a stalled daemon (MGIT-133) and a
	// wire-version mismatch (MGIT-136). A default that asserts a specific
	// diagnosis is wrong every time reality adds a case; a default that reports
	// what is known and names no cause is only ever incomplete.
	// Refs: MGIT-118, MGIT-136, MGIT-133, MGIT-104
	phaseUnidentified guestPhase = iota
	// phaseNeverStarted: the VM (or its guest agent) never answered on the
	// control channel, so no command ran inside it. The workload is not
	// implicated and neither, by construction, is in-guest memory exhaustion.
	phaseNeverStarted
	// phaseLostServing: the guest was reached and then stopped answering
	// mid-command — the shape real in-guest memory exhaustion takes, because
	// the kernel OOM killer takes the supervisor that would have reported it
	// (ADR-014). This is the phase the MGIT-95 cap advisory was built for, and
	// since MGIT-118 it is claimed only on positive evidence (lostServingMarkers)
	// rather than inherited by everything left over.
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
	// phaseAdmissionRefused: the HOST refused to admit the sandbox against its
	// aggregate ceiling, so no VM was ever attempted. Neither the workload nor
	// in-guest memory is implicated — there was no guest — and the fix is host
	// capacity, which is the OPPOSITE of the memory-cap advisory's: raising
	// this sandbox's --memory-mb makes a host-wide refusal more likely, not
	// less. That inversion, printed two lines under a refusal that says "this
	// launch is not too big", is what MGIT-118 was filed for.
	// Refs: MGIT-118, MGIT-98, FR-17.26
	phaseAdmissionRefused
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
// The evidence is the error mgit is about to print, and EVERY phase below is
// reached only by evidence that positively supports it. Nothing is reached by
// elimination: a failure matching none of them is phaseUnidentified, which
// diagnoses nothing. That inversion is the MGIT-118 fix — while the leftover
// branch named a specific cause, every failure shape the classifier had not
// yet met was reported to a user as in-guest memory exhaustion, four times
// over (MGIT-104, MGIT-118, MGIT-133, MGIT-136).
//
// The order is a precedence, not a preference. A version skew means the peers
// never transacted, so any other signal riding in the same message describes
// something that did not happen; a daemon stall and an admission refusal are
// likewise statements about the HOST that say nothing about a guest, so both
// are settled before any guest-shaped branch can reach a conclusion from them.
//
// Each sentinel is matched by TEXT as well as by errors.Is because an exec
// error crosses the daemon control protocol as a string (the result frame
// carries no error identity), so the in-process wrapping is gone by the time
// the CLI sees it. Refs: MGIT-118, MGIT-136, MGIT-133, MGIT-104, MGIT-92
func classifyGuestFailure(err error, ent entitlementState) guestFailure {
	if err == nil {
		return guestFailure{phase: phaseUnidentified}
	}
	switch {
	case isVersionSkew(err):
		return guestFailure{phase: phaseVersionSkew}
	case isDaemonStall(err):
		return guestFailure{phase: phaseDaemonStalled}
	case isAdmissionRefused(err):
		return guestFailure{phase: phaseAdmissionRefused}
	}
	if detail := vmStartFailure(err.Error()); detail != "" {
		return guestFailure{phase: phaseNeverStarted, startDetail: detail, entitlement: ent}
	}
	if isGuestNotServing(err) {
		return guestFailure{phase: phaseNeverStarted, entitlement: ent}
	}
	if isLostServing(err) {
		return guestFailure{phase: phaseLostServing}
	}
	return guestFailure{phase: phaseUnidentified}
}

// isAdmissionRefused reports whether a launch was refused by the host's
// aggregate ceiling before any VM was attempted.
//
// It is a fact about the HOST's capacity, never about this sandbox's size —
// the ceiling's own message says so ("this launch is not too big") — so it
// must be settled before any branch that could reach the memory-cap advisory.
// A per-sandbox limit refusal (model.ErrSandboxResourceLimitExceeded) is
// deliberately NOT matched here: that one really is "this launch is too big"
// and has the opposite fix, and it is refused at launch rather than arriving
// down the exec path.
//
// Matched by text as well as by errors.Is, for the same reason every sentinel
// here is: the refusal reaches the CLI through a daemon result frame that
// carries a string and no error identity. Refs: MGIT-118, MGIT-98, FR-17.26
func isAdmissionRefused(err error) bool {
	return err != nil && (errors.Is(err, model.ErrSandboxCeilingExceeded) ||
		strings.Contains(err.Error(), model.ErrSandboxCeilingExceeded.Error()))
}

// isGuestNotServing reports the launch fail-closed sentinel: the VMM started
// but the guest never answered, so the sandbox was torn down rather than
// reported running. Matched by text as well as by errors.Is, for the reason
// given on classifyGuestFailure. Refs: MGIT-104, MGIT-92
func isGuestNotServing(err error) bool {
	return err != nil && (errors.Is(err, model.ErrGuestNotServing) ||
		strings.Contains(err.Error(), model.ErrGuestNotServing.Error()))
}

// lostServingMarkers are the transport failures that only a connection to a
// guest which HAD been reached can produce: a stream that ended mid-frame, a
// peer that reset or closed it, or a re-dial refused by a guest that was
// answering a moment ago. Their presence is the positive evidence for
// phaseLostServing, which since MGIT-118 is claimed rather than inherited.
//
// The list is deliberately short, and what it leaves out is the point. A read
// deadline expiring ("i/o timeout") is NOT here: the host waited and gave up,
// which is a statement about the host's patience and not about the guest —
// yet as the leftover branch it was reported as in-guest memory exhaustion on
// a sandbox that was perfectly healthy (MGIT-122). Anything not listed lands
// in phaseUnidentified, where a missing marker costs a reader some evidence
// but never sends them after the wrong fix. Refs: MGIT-118, MGIT-122, MGIT-95
var lostServingMarkers = []string{
	"EOF",
	"connection reset",
	"broken pipe",
	"use of closed network connection",
	"connection refused",
}

// isLostServing reports whether the evidence shows a guest that was serving
// and was then lost — the one phase the MGIT-95 cap advisory is written for.
// Refs: MGIT-118, MGIT-95, R-H212
func isLostServing(err error) bool {
	if err == nil {
		return false
	}
	text := err.Error()
	for _, marker := range lostServingMarkers {
		if strings.Contains(text, marker) {
			return true
		}
	}
	return false
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
//
// Every phase is named explicitly and the default renders NOTHING but the
// honest report, so adding a phase without a renderer degrades to "mgit could
// not identify this" rather than to whichever diagnosis happened to sit in the
// default branch. Refs: MGIT-118, MGIT-136, MGIT-133, MGIT-104
func writeGuestFailure(w io.Writer, info *model.SandboxInfo, f guestFailure) {
	switch f.phase {
	case phaseVersionSkew:
		writeVersionSkew(w)
	case phaseDaemonStalled:
		writeDaemonStall(w, info)
	case phaseAdmissionRefused:
		writeAdmissionRefused(w, info)
	case phaseNeverStarted:
		writeStartFailure(w, info, f)
	case phaseLostServing:
		writeCapAdvisory(w, info, "the guest stopped answering mid-command")
	case phaseUnidentified:
		writeUnidentified(w, info)
	default:
		// A phase added without a renderer lands here, and lands honestly.
		writeUnidentified(w, info)
	}
}

// writeAdmissionRefused reports a launch the host refused for want of
// capacity, and exists to contradict nothing above it.
//
// The refusal itself was already printed — it travels inside the error this
// follows, naming the ceiling and the arithmetic — so this adds only what that
// message cannot say: that NO VM was started, and therefore that this
// sandbox's memory cap is not implicated. It names the inverted fix explicitly
// rather than merely omitting it, because the omission is not what an agent
// needs: MGIT-118 printed `--memory-mb 1024` two lines under a refusal that
// said "this launch is not too big", and an agent that has read that advice
// once needs to be told, in terms, that it is backwards.
// Refs: MGIT-118, MGIT-98, FR-17.26
func writeAdmissionRefused(w io.Writer, info *model.SandboxInfo) {
	_, _ = fmt.Fprintf(w, "\nmgit: the host refused to admit this sandbox%s, so no VM was started and no "+
		"command ran — this is the HOST's capacity, not a fault of your workload or of a guest.\n", taskSuffix(info))
	_, _ = fmt.Fprint(w, "This sandbox's own size is not the problem and raising it makes this refusal MORE likely, "+
		"not less: the ceiling counts memory across every sandbox on the host. Do not resize this sandbox "+
		"and do not reshape the build.\n")
	_, _ = fmt.Fprint(w, "Free host capacity instead: `mgit sandbox list` shows what is holding it, and "+
		"`mgit sandbox remove <task>` releases one. Then retry this command unchanged.\n")
}

// writeUnidentified reports a failure mgit could not place — and reports that
// it could not place it.
//
// This is what the classifier's default became after four separate causes were
// each, in turn, reported to a user as in-guest memory exhaustion because they
// were unrecognized (MGIT-104, MGIT-118, MGIT-133, MGIT-136). The value here
// is entirely in what it does NOT say: no phase, no cause, and above all no
// remedy, since a remedy for an unknown cause is the thing that costs a reader
// an afternoon. What it can offer honestly is the evidence and where to look
// next. Refs: MGIT-118
func writeUnidentified(w io.Writer, info *model.SandboxInfo) {
	_, _ = fmt.Fprintf(w, "\nmgit: mgit could not identify what failed here%s, and is reporting that rather "+
		"than guessing.\n", taskSuffix(info))
	_, _ = fmt.Fprint(w, "The error above is the whole of the evidence. NOT established, and so not claimed: "+
		"whether the guest ever received your command, whether it was running when this happened, and "+
		"whether this sandbox's caps are involved at all — so do not resize the sandbox or reshape the "+
		"build on the strength of this message.\n")
	_, _ = fmt.Fprintf(w, "Next: `mgit sandbox status %s` reports the state, backend and caps in force, and "+
		"`mgit sandbox list` shows whether the daemon is answering at all. A failure mgit cannot place is "+
		"worth reporting.\n", taskName(info))
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
