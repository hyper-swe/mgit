package libkrun

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"os"
	"time"

	"github.com/hyper-swe/mgit/internal/model"
	"github.com/hyper-swe/mgit/internal/sandboxd/egress"
)

// ChildCommand is the hidden mgit-sandboxd subcommand that runs ONE libkrun
// microVM and nothing else. It exists because krun_start_enter never returns:
// it seizes the process's stdio and exit()s with the guest's exit code at VM
// shutdown, so a VM started inside the daemon would kill the daemon — and
// every other sandbox — when its guest stopped. The daemon therefore re-execs
// ITSELF (os.Executable + this subcommand) per VM: no second binary to build,
// sign or ship, and the macOS hypervisor entitlement — attached to the signed
// daemon binary — carries over by construction. Refs: ADR-010, MGIT-61.8
const ChildCommand = "__krun-vm"

// handshakeFD is the file descriptor the child reports configuration progress
// on: fd 3, the first slot after stdio (the parent passes it via ExtraFiles).
// Stdio cannot carry it — stdin holds the spec and libkrun seizes
// stdin/stdout at boot, handing them to the guest.
const handshakeFD = 3

// childHandshake is one progress report on the handshake pipe, JSON lines.
// The first line reports configuration: ok means "configured, entering the
// VM" (written immediately before krun_start_enter). A LATER error line means
// the boot itself failed pre-guest — after it, the child's exit code is
// meaningless as a workload exit.
type childHandshake struct {
	OK    bool   `json:"ok"`
	Error string `json:"error,omitempty"`
}

// ChildMain is the child-process entry point behind ChildCommand. It reads
// the VM spec from stdin, configures the microVM, reports on the handshake
// pipe, and enters the VM — on success this call never returns control in a
// meaningful way (libkrun exit()s the process with the guest's exit code).
// The returned code is therefore always a FAILURE exit. Logs go to stderr as
// JSON; the parent routes them to the per-VM console log.
//
// The handshake pipe is DELIBERATELY never closed: its EOF is the child's
// process exit, which is exactly the signal the parent's wait path keys on —
// and in a mis-invoked process (run by hand, no ExtraFiles) fd 3 can be a
// live runtime descriptor, where a stray close is fatal to the Go runtime.
// Writes to a bad fd fail harmlessly and are logged.
func ChildMain(stdin io.Reader, stderr io.Writer) int {
	return childMain(stdin, os.NewFile(handshakeFD, "krun-handshake"), stderr)
}

// childMain is ChildMain minus the fd wiring, so the sequence is testable
// in-process without touching real file descriptors.
func childMain(stdin io.Reader, handshake io.Writer, stderr io.Writer) int {
	logger := slog.New(slog.NewJSONHandler(stderr, nil))

	// Spec first: a malformed spec reports the same way whether or not this
	// build carries the libkrun binding.
	spec, err := decodeSpec(stdin)
	if err != nil {
		return childFail(handshake, logger, err)
	}
	api, err := newPlatformAPI()
	if err != nil {
		return childFail(handshake, logger, err)
	}
	clock := func() time.Time { return time.Now().UTC() }
	return childRun(api, spec, handshake, logger, clock)
}

// childRun configures and enters one microVM. Split from ChildMain so the
// whole sequence — policy assembly, context configuration, handshake
// protocol, fail-closed teardown — is testable with a fake krunAPI on hosts
// without libkrun.
func childRun(api krunAPI, spec vmSpec, handshake io.Writer, logger *slog.Logger, clock func() time.Time) int {
	auth, dns, err := childPolicy(spec, logger, clock)
	if err != nil {
		return childFail(handshake, logger, err)
	}
	gc, err := newGuestCtx(api, spec, netDeps{auth: auth, dns: dns, logger: logger})
	if err != nil {
		return childFail(handshake, logger, err)
	}

	// "Configured, entering": the parent treats the VM as started once this
	// line arrives. Anything after it is a boot failure, reported as a SECOND
	// handshake line so the parent never mistakes it for a workload exit.
	writeHandshake(handshake, logger, childHandshake{OK: true})
	logger.Info("libkrun vm entering", "event", "krun_vm_enter",
		"sandbox_id", spec.SandboxID, "network_mode", spec.NetworkMode)

	// Never returns on success (ADR-010): libkrun exit()s with the guest's
	// exit code. A return is a pre-boot failure; gc is already released.
	err = gc.enter()
	return childFail(handshake, logger, err)
}

// childFail reports a failure on the handshake pipe and the log, and returns
// the child's failure exit code.
func childFail(handshake io.Writer, logger *slog.Logger, err error) int {
	logger.Error("libkrun vm failed", "event", "krun_vm_failed", "error", err.Error())
	writeHandshake(handshake, logger, childHandshake{Error: err.Error()})
	return 1
}

// writeHandshake best-effort writes one handshake line: a broken pipe (the
// parent died) must not stop the child's own teardown, but is logged.
func writeHandshake(w io.Writer, logger *slog.Logger, h childHandshake) {
	if w == nil {
		return
	}
	if err := json.NewEncoder(w).Encode(h); err != nil {
		logger.Warn("libkrun handshake write failed", "event", "krun_handshake_writefail",
			"error", err.Error())
	}
}

// childPolicy assembles the egress policy enforcement for the child's own
// netstack gateway:
//
//   - allowlist gets the standard per-sandbox egress assembly (allowlist
//     compile, pinned resolver, authorizer, DNS server) — the SAME policy
//     code every backend uses; only the data path (netstack vs CONNECT
//     proxy) differs.
//   - open gets an allow-all authorizer, so it is unrestricted but still
//     AUDITED per flow — something the iptables NAT path could not do.
//   - none runs no gateway at all (the NIC gets the discard socket), so it
//     needs no policy.
//
// The child audits egress decisions to its own structured log (the per-VM
// console log). The daemon's durable sandbox_events store is NOT reachable
// from the child process yet — a known gap, tracked in MGIT-61.9.
// Refs: SEC-04, FR-17.8, ADR-010
func childPolicy(spec vmSpec, logger *slog.Logger, clock func() time.Time) (flowAuthorizer, dnsResolver, error) {
	switch spec.NetworkMode {
	case model.NetworkModeNone:
		return nil, nil, nil
	case model.NetworkModeOpen:
		// Open places no restriction on destinations, but it still gets an
		// authorizer — the gateway refuses to run without one, and this is
		// what makes open mode AUDITED per flow, which the iptables NAT path
		// it replaces could not do. No DNS resolver: open mode connects by
		// address and the guest's own resolution is unrestricted.
		auth, err := egress.NewOpenAuthorizer(egress.OpenAuthorizerConfig{
			SandboxID: spec.SandboxID, TaskID: spec.TaskID,
			Audit: logAuditor{logger: logger}, Logger: logger,
		})
		if err != nil {
			return nil, nil, fmt.Errorf("libkrun child open-mode egress: %w", err)
		}
		return auth, nil, nil
	}
	// Allowlist: the standard per-sandbox assembly.
	sup, err := egress.NewSupervisor(egress.SupervisorConfig{
		SandboxID: spec.SandboxID,
		TaskID:    spec.TaskID,
		Policy:    model.NetworkPolicy{Mode: spec.NetworkMode, Allowlist: spec.Allowlist},
		Audit:     logAuditor{logger: logger},
		Lookup:    egress.SystemLookup(nil),
		Dial:      egress.HostDial,
		Clock:     clock,
		Logger:    logger,
	})
	if err != nil {
		return nil, nil, fmt.Errorf("libkrun child egress assembly: %w", err)
	}
	return sup.Authorizer(), sup.DNS(), nil
}

// logAuditor satisfies egress.Auditor by writing each egress decision to the
// child's structured log (routed to the per-VM console log by the parent).
// It exists because the durable audit store lives in the daemon process and
// no channel carries records across the VM process boundary yet (MGIT-61.9);
// losing the records entirely — or refusing allowlist mode — would both be
// worse. Refs: FR-17.8
type logAuditor struct {
	logger *slog.Logger
}

// AppendEgressRecord logs one egress decision record.
func (a logAuditor) AppendEgressRecord(_ context.Context, rec *model.EgressRecord) error {
	a.logger.Info("egress decision", "event", "egress_record",
		"sandbox_id", rec.SandboxID, "task_id", rec.TaskID, "decision", rec.Decision,
		"protocol", rec.Protocol, "dest_host", rec.DestHost, "dest_ip", rec.DestIP,
		"dest_port", rec.DestPort, "rule", rec.Rule)
	return nil
}
