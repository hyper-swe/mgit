package sandboxd

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"strings"

	"github.com/hyper-swe/mgit/internal/controlproto"
	"github.com/hyper-swe/mgit/internal/execwire"
	"github.com/hyper-swe/mgit/internal/model"
)

// SandboxDispatcher is the subset of the sandbox service the daemon's
// request handlers invoke. Handlers go through this service, never the
// manager directly (architecture rule); *service.SandboxService satisfies
// it. The daemon owns this narrow interface so it depends on a contract,
// not a concrete service type. Refs: FR-17.16, MGIT-11.10.8
type SandboxDispatcher interface {
	Register(ctx context.Context, opts model.SandboxLaunchOptions) (*model.SandboxInfo, error)
	Exec(ctx context.Context, taskID string, req model.ExecRequest) (*model.ExecResult, error)
	List(ctx context.Context) ([]model.SandboxInfo, error)
	Remove(ctx context.Context, taskID string, force bool) error
	Status(ctx context.Context, taskID string) (*model.SandboxInfo, error)
	// SyncWorktree re-stages a task's host worktree into its running guest,
	// or (DryRun) reports the classification without touching it. It is part
	// of the core verb set rather than an optional collaborator because a
	// daemon that serves exec MUST be able to answer "is the guest running
	// the code the host has" — the backend, not the daemon, is where the
	// capability can legitimately be absent. Refs: MGIT-76, ADR-011
	SyncWorktree(ctx context.Context, taskID string, opts model.WorktreeSyncOptions) (*model.WorktreeSyncReport, error)
}

// SandboxLander serves the land verb. It is the daemon's ENTIRE land
// capability: "land this task" — pull the guest pool, verify it host-side,
// and import atomically. The implementation (service.LandService, wrapped at
// wiring) routes exclusively through the verified LandOrchestrator, so the
// daemon can never import guest objects without verification and holds no
// persister/importer/brancher reference. Commits is the number of new
// commits landed (0 = nothing new); Branch is the task branch advanced.
// Refs: MGIT-11.10.10, SEC-01
type SandboxLander interface {
	Land(ctx context.Context, taskID string) (commits int, branch string, err error)
}

// GrantCoordinator serves the capability-escalation control verbs: list a
// sandbox's pending capability requests (derived host-side from observed
// denials, SEC-05) and approve one into a live, audited grant.
// *service.CapabilityService satisfies it; keyed by host-owned sandbox ID
// (the dispatch resolves task->sandbox). Refs: FR-17.12, SEC-05
type GrantCoordinator interface {
	PendingRequests(sandboxID string) []model.CapabilityRequest
	Approve(ctx context.Context, sandboxID, key string) (*model.CapabilityGrant, error)
}

// PolicyCoordinator serves the live egress-policy verbs: replace a RUNNING
// sandbox's allowlist without relaunching it, and report the one in force.
// *service.EgressPolicyService satisfies it.
//
// It is keyed by the RESOLVED sandbox binding, not by the task ID on the
// request: the dispatch resolves task->sandbox through the service first, so
// the sandbox a mutation lands on is always host-anchored (SEC-05).
// Refs: MGIT-72, SEC-04, SEC-05
type PolicyCoordinator interface {
	Set(ctx context.Context, info model.SandboxInfo, entries []string, drain bool) (*model.EgressPolicyChange, error)
	Show(ctx context.Context, info model.SandboxInfo) (*model.EgressPolicyState, error)
}

// ArtifactExporter serves the guest->host artifact export verb (MGIT-73):
// "export this host-named path out of this task's sandbox to this host-named
// destination". *service.SandboxService satisfies it.
//
// It is a SEPARATE seam from SandboxDispatcher, like SandboxLander, because
// the capability is optional: a daemon whose backend delivers the worktree as
// a launch-time image has no host directory to export from and leaves this
// unwired, and the verb then reports itself unserved rather than pretending.
// Refs: MGIT-73, ADR-011
type ArtifactExporter interface {
	ExportArtifact(ctx context.Context, taskID string,
		req model.ArtifactExportRequest) (*model.ArtifactExportResult, error)
}

// execRelayChunkBytes bounds one exec output frame relayed to the client.
// Output is forwarded in chunks no larger than this so a single frame can
// never approach the execwire ceiling and the client sees output
// incrementally. Refs: MGIT-11.10.8 (security audit: per-frame bound)
const execRelayChunkBytes = 64 << 10

// serveRequest reads and dispatches exactly one control-plane request on an
// already-authenticated, already-greeted connection. One request per
// connection: the CLI dials, performs one operation, and closes. A read
// deadline bounds a slow-loris client, and a malformed/oversized request
// fails closed (logged, best-effort error reply) without disturbing the
// daemon. A greet-only build (no service wired) serves nothing.
// Refs: FR-17.34, MGIT-11.10.8
func (d *Daemon) serveRequest(ctx context.Context, conn net.Conn) {
	req, ok := d.readRequest(conn)
	if !ok {
		return
	}
	// A hello where a verb belongs is not a verb. The handshake happens once,
	// before anything is transacted, and repeating it is malformed traffic
	// rather than a second chance to renegotiate. Refs: MGIT-136
	if req.Kind == controlproto.KindHello {
		d.cfg.Logger.Warn("sandboxd rejected request",
			"event", "request_rejected", "error", "hello after the handshake")
		d.writeResponse(conn, &controlproto.Response{Error: "invalid request"})
		return
	}
	d.dispatch(ctx, conn, req)
}

// readRequest reads and validates exactly one control frame under a read
// deadline, reporting whether one was obtained. A clean EOF is the benign
// greeting-probe close (activation health check) and is silent; anything else
// is a malformed/oversized/slow client, which fails closed with an audited
// rejection and a best-effort reply. Refs: FR-17.34, MGIT-11.10.8
func (d *Daemon) readRequest(conn net.Conn) (*controlproto.Request, bool) {
	_ = conn.SetReadDeadline(d.cfg.Clock().Add(controlproto.DefaultRequestTimeout))
	req, err := controlproto.ReadRequest(conn)
	if err != nil {
		if !errors.Is(err, io.EOF) {
			d.cfg.Logger.Warn("sandboxd rejected request",
				"event", "request_rejected", "error", err.Error())
			d.writeResponse(conn, &controlproto.Response{Error: "invalid request"})
		}
		return nil, false
	}
	return req, true
}

// dispatch routes one validated request to the service and replies. Exec
// streams execwire frames; every other kind replies with one control
// response. Refs: FR-17.34, MGIT-11.10.8
func (d *Daemon) dispatch(ctx context.Context, conn net.Conn, req *controlproto.Request) {
	switch req.Kind {
	case controlproto.KindLaunch:
		info, err := d.cfg.Service.Register(ctx, *req.Launch)
		d.reply(conn, &controlproto.Response{Sandbox: info}, err)
	case controlproto.KindExec:
		d.serveExec(ctx, conn, req.Exec)
	case controlproto.KindList:
		list, err := d.cfg.Service.List(ctx)
		d.reply(conn, &controlproto.Response{List: list}, err)
	case controlproto.KindRemove:
		err := d.cfg.Service.Remove(ctx, req.Remove.TaskID, req.Remove.Force)
		d.reply(conn, &controlproto.Response{}, err)
	case controlproto.KindStatus:
		info, err := d.cfg.Service.Status(ctx, req.Status.TaskID)
		d.reply(conn, &controlproto.Response{Sandbox: info}, err)
	case controlproto.KindLand:
		d.serveLand(ctx, conn, req.Land)
	case controlproto.KindGrants:
		d.serveGrants(ctx, conn, req.Grants)
	case controlproto.KindGrant:
		d.serveGrant(ctx, conn, req.Grant)
	case controlproto.KindSync:
		d.serveSync(ctx, conn, req.Sync)
	case controlproto.KindPolicySet:
		d.servePolicySet(ctx, conn, req.PolicySet)
	case controlproto.KindPolicyShow:
		d.servePolicyShow(ctx, conn, req.PolicyShow)
	case controlproto.KindExport:
		d.serveExport(ctx, conn, req.Export)
	case controlproto.KindEcho:
		d.serveEcho(conn, req.Echo)
	default:
		d.reply(conn, &controlproto.Response{},
			fmt.Errorf("controlproto kind %#x not served by this daemon", req.Kind))
	}
}

// serveLand routes one land request through the verified land path. The
// daemon's only land dependency is the SandboxLander (land-this-task), which
// imports nothing without the orchestrator's host-side verification; the
// daemon never touches the persister or stores (SEC-01, no-bypass guard).
// A nil lander (land not wired) is reported, not crashed. The reply carries
// only host-observed text (SEC-05). Refs: MGIT-11.10.10, SEC-01, SEC-05
func (d *Daemon) serveLand(ctx context.Context, conn net.Conn, ref *controlproto.TaskRef) {
	if d.cfg.Lander == nil {
		d.reply(conn, &controlproto.Response{}, d.unservedVerb(ctx, ref.TaskID, "land",
			"no land path is wired, so commits cannot be brought back from the guest through it — "+
				"the host repository was not reachable when this daemon started, or this build carries no land transport",
			"bring files out with `mgit sandbox export`, or restart the daemon from the repository "+
				"(`mgit sandbox stop` then any sandbox verb)",
			"nothing was landed and the guest's history is intact"))
		return
	}
	commits, branch, err := d.cfg.Lander.Land(ctx, ref.TaskID)
	d.reply(conn, &controlproto.Response{Landed: &controlproto.LandResult{Commits: commits, Branch: branch}}, err)
}

// serveGrants lists a task's pending capability requests for operator review.
// It resolves task->sandbox via the service (host-anchored, never guest text),
// then returns the host-observed pending requests. A nil coordinator (grants
// not wired, e.g. off Linux) reports the verb as unserved. Refs: FR-17.12, SEC-05
func (d *Daemon) serveGrants(ctx context.Context, conn net.Conn, ref *controlproto.TaskRef) {
	if d.cfg.Grants == nil || d.cfg.Service == nil {
		d.reply(conn, &controlproto.Response{}, d.unservedVerb(ctx, ref.TaskID, "grants",
			grantsUnwiredFact, grantsInstead, "no request was listed or approved"))
		return
	}
	info, err := d.cfg.Service.Status(ctx, ref.TaskID)
	if err != nil {
		d.reply(conn, &controlproto.Response{}, err)
		return
	}
	reqs := d.cfg.Grants.PendingRequests(info.ID)
	pending := make([]controlproto.PendingGrant, 0, len(reqs))
	for _, r := range reqs {
		pending = append(pending, controlproto.PendingGrant{
			Capability: r.Capability, DestIP: r.ObservedDestIP, DestPort: r.ObservedDestPort, Key: r.Key(),
		})
	}
	d.reply(conn, &controlproto.Response{Pending: pending}, nil)
}

// serveGrant approves one pending capability request, turning it into a live,
// audited, sandbox-lifetime-scoped grant. Refs: FR-17.12, SEC-05
func (d *Daemon) serveGrant(ctx context.Context, conn net.Conn, args *controlproto.GrantArgs) {
	if d.cfg.Grants == nil || d.cfg.Service == nil {
		d.reply(conn, &controlproto.Response{}, d.unservedVerb(ctx, args.TaskID, "grant",
			grantsUnwiredFact, grantsInstead, "no request was approved and no egress was opened"))
		return
	}
	info, err := d.cfg.Service.Status(ctx, args.TaskID)
	if err != nil {
		d.reply(conn, &controlproto.Response{}, err)
		return
	}
	grant, err := d.cfg.Grants.Approve(ctx, info.ID, args.Key)
	if err != nil {
		d.reply(conn, &controlproto.Response{}, err)
		return
	}
	d.reply(conn, &controlproto.Response{Granted: &controlproto.GrantResult{
		Capability: grant.Capability, DestIP: grant.ObservedDestIP, DestPort: grant.ObservedDestPort,
	}}, nil)
}

// serveSync re-stages a task's host worktree into its running guest, or
// reports the classification for a dry run.
//
// It does NOT use reply(), which collapses a failed op to its error string.
// A conflict refusal must carry BOTH: the error, so the caller cannot mistake
// it for success, and the classification, so the refusal names every diverged
// path. Discovering those paths is the capability this verb adds, and making a
// caller re-derive them would mean asking again against a tree that may
// already have moved. Every field is host-computed (SEC-05).
// Refs: MGIT-76, ADR-011
func (d *Daemon) serveSync(ctx context.Context, conn net.Conn, args *controlproto.SyncArgs) {
	report, err := d.cfg.Service.SyncWorktree(ctx, args.TaskID, args.Sync)
	// Bound the report before it crosses the wire. A worktree holding a
	// host-side node_modules classifies tens of thousands of paths, and the
	// whole answer used to be dropped for exceeding the response limit — the
	// caller got EOF and the reason stayed in the daemon log. The bounded form
	// carries full COUNTS and marks itself truncated, so it is always sendable
	// and never mistakable for a complete list. Refs: MGIT-160
	if report != nil {
		bounded := report.Bound(model.SyncReportPathLimit)
		report = &bounded
	}
	resp := &controlproto.Response{Synced: report}
	if err != nil {
		d.cfg.Logger.Warn("sandboxd op failed", "event", "op_error", "error", err.Error())
		resp.Error = err.Error()
	}
	d.writeResponse(conn, resp)
}

// unservedVerb builds the refusal for an optional verb this daemon cannot
// serve, in the operator's words: the verb, the fact about this daemon or
// backend that makes it unservable, what to do instead, and that nothing
// happened. Modeled on policyUnservedReason, which MGIT-104 fixed while its
// four siblings still answered with a hex opcode — "kind 0x44" tells a reader
// neither that THIS BUILD cannot serve the verb (stop trying) nor that THIS
// CALL failed (retry), and those need opposite responses. Refs: MGIT-171, MGIT-104
func (d *Daemon) unservedVerb(ctx context.Context, taskID, verb, fact, instead, untouched string) error {
	return fmt.Errorf("`mgit sandbox %s` is not served by this daemon on %s: %s. %s. "+
		"This is a refusal, not a failure to reach the daemon: nothing was changed — %s",
		verb, d.backendName(ctx, taskID), fact, sentence(instead), untouched)
}

// sentence makes a clause read as the sentence it is placed as: the "what to
// do instead" clause follows a full stop, so its first letter is upper-cased
// unless it opens with a quoted command. Refs: MGIT-171
func sentence(clause string) string {
	if clause == "" || clause[0] == '`' {
		return clause
	}
	return strings.ToUpper(clause[:1]) + clause[1:]
}

// backendName names the task's backend when the daemon can look it up, and
// otherwise says honestly that it is this build's backend.
func (d *Daemon) backendName(ctx context.Context, taskID string) string {
	if d.cfg.Service != nil && taskID != "" {
		if info, err := d.cfg.Service.Status(ctx, taskID); err == nil && info.Backend != "" {
			return info.Backend
		}
	}
	return "this build's sandbox backend"
}

const (
	grantsUnwiredFact = "no grant coordinator is wired, so there are no pending egress requests " +
		"to list or approve — grants exist only where this daemon enforces the allowlist itself " +
		"(firecracker on Linux)"
	grantsInstead = "set the allowlist up front with `mgit sandbox policy set`, or launch with " +
		"--network open if the task genuinely needs unrestricted egress"
)

// exportUnservedReason explains why nothing can leave this sandbox through
// `mgit sandbox export`. On firecracker the reason is a property of the
// backend, not of the wiring: the worktree was delivered as a launch-time
// image, so there is no host directory to read an artifact from.
// Refs: MGIT-171, MGIT-73, FR-17.18
func (d *Daemon) exportUnservedReason(ctx context.Context, taskID string) error {
	fact := "no exporter is wired, so artifacts cannot be copied out of the guest"
	instead := "bring commits back with `mgit sandbox land`, or run a daemon built with the export path"
	if d.backendName(ctx, taskID) == "firecracker" {
		fact = "firecracker delivers the worktree as a launch-time image, so there is no host " +
			"directory to read an artifact from"
		instead = "bring commits back with `mgit sandbox land`, or re-launch the task on a backend " +
			"whose guest tree lives on the host (libkrun or vzf)"
	}
	return d.unservedVerb(ctx, taskID, "export", fact, instead, "nothing left the sandbox")
}

// policyUnservedReason explains, in CONTAINMENT terms, why this daemon serves
// no live egress-policy verbs.
//
// The old reply was "controlproto kind 0x51 not served by this daemon". That
// describes the daemon's wiring; it does not tell an operator the fact that
// matters before they run untrusted code — that this backend enforces no live
// allowlist, so there is no policy here to show or change. Those are different
// facts, and only the second is actionable. Refs: MGIT-111, MGIT-104, SEC-04
func (d *Daemon) policyUnservedReason(ctx context.Context, taskID string) error {
	backend := "this build's sandbox backend"
	if d.cfg.Service != nil && taskID != "" {
		if info, err := d.cfg.Service.Status(ctx, taskID); err == nil && info.Backend != "" {
			backend = info.Backend
		}
	}
	return fmt.Errorf(
		"no live egress allowlist is enforced on %s, so there is no policy to show or change: "+
			"this daemon has no egress enforcer wired, and a sandbox on this backend cannot enforce "+
			"an allowlist at all. Launch with --network none for no egress, or use a backend that "+
			"enforces one (libkrun on macOS, firecracker on Linux). This is a refusal, not a failure "+
			"to reach the daemon: nothing was changed",
		backend)
}

// servePolicySet replaces a running sandbox's egress allowlist. An empty entry
// list is a full revoke.
//
// A nil coordinator reports the verb UNSERVED rather than replying success: a
// daemon with no enforcer wired that answered "revoked" would leave the caller
// running untrusted code believing egress was closed — the worst possible lie
// this verb can tell. Refs: MGIT-72, SEC-04
func (d *Daemon) servePolicySet(ctx context.Context, conn net.Conn, args *controlproto.PolicyArgs) {
	if d.cfg.Policy == nil || d.cfg.Service == nil {
		d.reply(conn, &controlproto.Response{}, d.policyUnservedReason(ctx, args.TaskID))
		return
	}
	info, err := d.cfg.Service.Status(ctx, args.TaskID)
	if err != nil {
		d.reply(conn, &controlproto.Response{}, err)
		return
	}
	change, err := d.cfg.Policy.Set(ctx, *info, args.Entries, args.Drain)
	if err != nil {
		d.reply(conn, &controlproto.Response{}, err)
		return
	}
	d.reply(conn, &controlproto.Response{Policy: &controlproto.PolicyResult{
		Entries: change.Entries, RuleCount: change.RuleCount,
		Killed: change.Killed, Drained: change.Drained,
		// A policy staged onto a sandbox whose VM has not booted crosses the
		// wire LABELED. Refs: MGIT-109
		Pending: change.Pending,
	}}, nil)
}

// servePolicyShow reports the egress policy a running sandbox is enforcing
// right now — which after a live mutation is NOT its launch-time policy.
// Refs: MGIT-72
func (d *Daemon) servePolicyShow(ctx context.Context, conn net.Conn, ref *controlproto.TaskRef) {
	if d.cfg.Policy == nil || d.cfg.Service == nil {
		d.reply(conn, &controlproto.Response{}, d.policyUnservedReason(ctx, ref.TaskID))
		return
	}
	info, err := d.cfg.Service.Status(ctx, ref.TaskID)
	if err != nil {
		d.reply(conn, &controlproto.Response{}, err)
		return
	}
	state, err := d.cfg.Policy.Show(ctx, *info)
	if err != nil {
		d.reply(conn, &controlproto.Response{}, err)
		return
	}
	d.reply(conn, &controlproto.Response{Policy: &controlproto.PolicyResult{
		Entries: state.Entries, RuleCount: state.RuleCount, Pending: state.Pending,
	}}, nil)
}

// serveExport routes one artifact export through the service, which resolves
// the task to its sandbox, applies the host-side containment checks in the
// backend and records the crossing in the append-only audit trail. The daemon
// itself touches no filesystem here. An unwired exporter is reported, not
// crashed. Refs: MGIT-73, FR-17.18, ADR-011
func (d *Daemon) serveExport(ctx context.Context, conn net.Conn, args *controlproto.ExportArgs) {
	if d.cfg.Exporter == nil {
		d.reply(conn, &controlproto.Response{}, d.exportUnservedReason(ctx, args.TaskID))
		return
	}
	res, err := d.cfg.Exporter.ExportArtifact(ctx, args.TaskID, args.Export)
	d.reply(conn, &controlproto.Response{Exported: res}, err)
}

// reply writes a success response, or an error response carrying a
// host-observed message (SEC-05: never guest-sourced text). A failed
// operation is audited operationally. Refs: MGIT-11.10.8
func (d *Daemon) reply(conn net.Conn, resp *controlproto.Response, opErr error) {
	if opErr != nil {
		code := failureCode(opErr)
		d.cfg.Logger.Warn("sandboxd op failed", "event", "op_error",
			"error", opErr.Error(), "error_code", code)
		resp = &controlproto.Response{Error: opErr.Error(), ErrorCode: code}
	}
	d.writeResponse(conn, resp)
}

// failureCode extracts the STABLE machine-readable failure token from an
// operation error, or "" for a verb with no code vocabulary.
//
// It is deliberately the only place a code reaches the wire, so a code can
// never be invented at a call site: it comes from the typed error the service
// built, validated against the closed set, or not at all. An integrator
// matching on this token never has to match on prose again.
// Refs: MGIT-109, R-H233
func failureCode(opErr error) string {
	var failure *model.EgressPolicyError
	if errors.As(opErr, &failure) && model.ValidEgressFailureCode(failure.Code) {
		return failure.Code
	}
	return ""
}

// armWriteDeadline bounds a single reply write so a stalled client cannot
// wedge a handler goroutine indefinitely.
func (d *Daemon) armWriteDeadline(conn net.Conn) {
	_ = conn.SetWriteDeadline(d.cfg.Clock().Add(controlproto.DefaultRequestTimeout))
}

// writeResponse sends one control response under a write deadline.
func (d *Daemon) writeResponse(conn net.Conn, resp *controlproto.Response) {
	d.armWriteDeadline(conn)
	err := controlproto.WriteResponse(conn, resp)
	if err == nil {
		return
	}
	d.cfg.Logger.Warn("sandboxd write response failed",
		"event", "write_error", "error", err.Error())

	// A response that cannot be SENT must still be a response.
	//
	// WriteResponse refuses anything over its size cap and writes NOTHING, so
	// this used to log and return — and the handler's deferred close then gave
	// the caller a bare "read response: EOF". Nothing on the wire, the only
	// record of the cause inside the daemon, and a client error naming neither
	// the cause nor a next step. A crash where a refusal belonged.
	//
	// Retrying with a SMALL error is the whole fix: the refusal is built from
	// nothing but a size and a cap, so it cannot fail the same way the payload
	// did. A second failure here is a dead connection, not an over-size one,
	// and there is nothing further to say into it. Refs: MGIT-160
	if !errors.Is(err, controlproto.ErrResponseTooLarge) {
		return
	}
	d.armWriteDeadline(conn)
	// No ErrorCode: the code vocabulary is a validated closed set, and
	// failureCode's own rule is that a code never gets invented at a call
	// site. Giving this failure a stable token is worth doing deliberately,
	// with the set, rather than smuggling one in here.
	if err := controlproto.WriteResponse(conn, &controlproto.Response{Error: err.Error()}); err != nil {
		d.cfg.Logger.Warn("sandboxd could not send the over-size refusal either",
			"event", "write_error", "error", err.Error())
	}
}

// serveExec runs one command through the service and relays the outcome as
// an execwire frame stream: stdout ('O') and stderr ('E') in bounded
// chunks, then the terminal result ('R') carrying the exit code. A setup
// failure is reported as a result frame with an error string so the
// client's frame reader always sees a clean end-of-stream.
//
// The stream OPENS with a liveness beat ('H') and keeps beating until the
// command finishes. Nothing else crosses this connection in between — the
// service returns a whole ExecResult, which is only relayed once the command
// has ended — so without the beats a client cannot tell a twenty-minute build
// from a daemon that stopped answering, and MGIT-122 established that killing
// the first to catch the second is not an acceptable trade. The FIRST beat is
// written before the service is called — before any lazy boot and before any
// guest work — so the client's very first idle window is judgeable: a peer that
// got past the version handshake has stated it speaks this beat, so silence in
// that window is a wedged daemon, not a slow command. That is what let MGIT-138
// delete the client's "perhaps it is only an old daemon" escape hatch.
// Refs: FR-17.11, MGIT-11.10.8, MGIT-138, MGIT-136, MGIT-133, MGIT-122
func (d *Daemon) serveExec(ctx context.Context, conn net.Conn, args *controlproto.ExecArgs) {
	if !d.writeHeartbeat(conn) {
		return // the client is already gone; there is nothing to serve
	}
	done := make(chan execOutcome, 1)
	go d.runExec(ctx, execArgs{taskID: args.TaskID, req: args.Exec}, done)
	out, connLive := d.beatWhileExecuting(ctx, conn, args.TaskID, done)
	if !connLive {
		return // the client hung up mid-command; every further write would fail
	}
	if out.err != nil {
		d.cfg.Logger.Warn("sandboxd exec failed", "event", "op_error", "error", out.err.Error())
		d.writeResultFrame(conn, execwire.Result{}, out.err.Error())
		return
	}
	d.armWriteDeadline(conn)
	if !d.relayChunks(conn, execwire.FrameStdout, out.res.Stdout) ||
		!d.relayChunks(conn, execwire.FrameStderr, out.res.Stderr) {
		return // the connection is gone; the result frame would also fail
	}
	d.writeResultFrame(conn, execwire.Result{ExitCode: out.res.ExitCode}, "")
}

// relayChunks writes data as execwire frames no larger than
// execRelayChunkBytes. It reports whether every frame was written.
func (d *Daemon) relayChunks(conn net.Conn, kind byte, data []byte) bool {
	for len(data) > 0 {
		n := min(len(data), execRelayChunkBytes)
		if err := execwire.WriteFrame(conn, kind, data[:n]); err != nil {
			d.cfg.Logger.Warn("sandboxd exec relay failed",
				"event", "write_error", "error", err.Error())
			return false
		}
		data = data[n:]
	}
	return true
}

// writeResultFrame writes the terminal execwire result frame.
func (d *Daemon) writeResultFrame(conn net.Conn, result execwire.Result, errStr string) {
	payload, err := json.Marshal(execwire.ResultFrame{Result: result, Error: errStr})
	if err != nil {
		d.cfg.Logger.Error("sandboxd encode result frame failed", "event", "write_error", "error", err.Error())
		return
	}
	d.armWriteDeadline(conn)
	if err := execwire.WriteFrame(conn, execwire.FrameResult, payload); err != nil {
		d.cfg.Logger.Warn("sandboxd write result frame failed", "event", "write_error", "error", err.Error())
	}
}
