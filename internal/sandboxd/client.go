package sandboxd

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"strings"
	"time"

	"github.com/hyper-swe/mgit/internal/controlproto"
	"github.com/hyper-swe/mgit/internal/execwire"
	"github.com/hyper-swe/mgit/internal/model"
)

// Client is the control-plane client the mgit CLI uses to drive the
// daemon. It dials a fresh connection per request (matching the daemon's
// one-request-per-connection model), verifies the liveness greeting (a
// socket that accepts but cannot greet is a squatter, not the daemon), and
// speaks internal/controlproto. The CLI talks to the daemon ONLY, never
// the Store or Manager (architecture rule).
//
// TRUST BOUNDARY: the socket is same-UID only (0600 in a 0700 dir); this
// client is as privileged as the daemon. The real security boundary is
// host<->guest, unchanged. Refs: FR-17.34, MGIT-11.10.9
type Client struct {
	socketPath string
	clock      func() time.Time
	// requestTimeout bounds ONE control-plane request/response exchange, armed
	// afresh for each phase it applies to (greeting, request write, response
	// read) rather than once at dial. It deliberately does NOT bound the exec
	// frame stream; see Exec. Refs: MGIT-122
	requestTimeout time.Duration
	// stallTimeout bounds SILENCE on the exec frame stream — the gap between
	// frames, never the command's duration. It is armed only once the daemon
	// has proved it beats. Refs: MGIT-133
	stallTimeout time.Duration
}

// NewClient returns a control-plane client for the daemon at socketPath.
func NewClient(socketPath string, clock func() time.Time) *Client {
	return &Client{
		socketPath:     socketPath,
		clock:          clock,
		requestTimeout: controlproto.DefaultRequestTimeout,
		stallTimeout:   execwire.StallTimeout,
	}
}

// dialGreeted dials the daemon and consumes its liveness greeting,
// returning a connection ready to carry one request. A socket that does
// not greet is rejected (squatter defense). The caller closes the conn.
//
// The greeting deadline is cleared before returning. What follows is a
// REQUEST, and its budget starts when that request is written — inheriting
// this one would silently charge the response for however long the greeting
// took. Refs: MGIT-122
func (c *Client) dialGreeted(ctx context.Context) (net.Conn, error) {
	var dialer net.Dialer
	conn, err := dialer.DialContext(ctx, "unix", c.socketPath)
	if err != nil {
		return nil, fmt.Errorf("sandbox client: dial daemon: %w", err)
	}
	_ = conn.SetReadDeadline(c.clock().Add(c.requestTimeout))
	buf := make([]byte, len(greeting))
	if _, err := io.ReadFull(conn, buf); err != nil || string(buf) != greeting {
		_ = conn.Close()
		return nil, fmt.Errorf("sandbox client: daemon did not greet (not running, or a squatter holds the socket)")
	}
	_ = conn.SetReadDeadline(time.Time{})
	return conn, nil
}

// expireNow is any instant safely in the past: setting it as a deadline makes
// an in-progress read or write return at once. Used to unblock a connection on
// cancellation, where "expire immediately" is the intent and the injected clock
// (which a test may freeze) is the wrong thing to ask.
var expireNow = time.Unix(1, 0)

// watchCancel makes ctx cancellation end a blocked read on conn, and returns a
// stop function the caller must defer.
//
// Without it a canceled caller is not actually canceled: it stays in the socket
// read until some deadline fires, which for the exec stream is now never. The
// connection is expired rather than closed so this never races the caller's own
// deferred Close. Refs: MGIT-122
func watchCancel(ctx context.Context, conn net.Conn) func() {
	if ctx.Done() == nil {
		return func() {}
	}
	stop := make(chan struct{})
	done := make(chan struct{})
	go func() {
		defer close(done)
		select {
		case <-ctx.Done():
			_ = conn.SetDeadline(expireNow)
		case <-stop:
		}
	}()
	return func() { close(stop); <-done }
}

// roundTrip sends one request and returns the daemon's response, mapping a
// response-level error to a Go error and DISCARDING the body. Used by the
// non-streaming verbs, whose payloads carry nothing useful on failure.
func (c *Client) roundTrip(ctx context.Context, req *controlproto.Request) (*controlproto.Response, error) {
	resp, err := c.roundTripRaw(ctx, req)
	if err != nil {
		return nil, err
	}
	if resp.Error != "" {
		return nil, remoteFailure(resp)
	}
	return resp, nil
}

// remoteFailure rebuilds a daemon-side failure on this side of the wire,
// PRESERVING its stable machine-readable code where the verb defines one.
//
// Without this the code would survive the daemon only as text inside a message,
// and every consumer would be back to matching on prose — the failure mode this
// contract exists to end. A coded failure comes back as a *model.EgressPolicyError,
// so callers use errors.As, not strings.Contains. Refs: MGIT-109, R-H233
func remoteFailure(resp *controlproto.Response) error {
	if model.ValidEgressFailureCode(resp.ErrorCode) {
		// The daemon already rendered the token into its text; strip it so
		// re-wrapping here does not print it twice. The token is carried
		// STRUCTURALLY either way — the text is a convenience.
		msg := strings.TrimPrefix(resp.Error, "["+resp.ErrorCode+"] ")
		return &model.EgressPolicyError{Code: resp.ErrorCode, Reason: "sandbox: " + msg}
	}
	return fmt.Errorf("sandbox: %s", resp.Error)
}

// roundTripRaw sends one request and returns the daemon's response VERBATIM,
// reporting only transport failures. A verb whose failure reply carries data
// the caller needs — sync, whose refusal names the conflicting paths — uses
// this and maps the response-level error itself. Refs: MGIT-76
func (c *Client) roundTripRaw(ctx context.Context, req *controlproto.Request) (*controlproto.Response, error) {
	conn, err := c.dialGreeted(ctx)
	if err != nil {
		return nil, err
	}
	defer func() { _ = conn.Close() }()
	defer watchCancel(ctx, conn)()

	_ = conn.SetWriteDeadline(c.clock().Add(c.requestTimeout))
	if err := controlproto.WriteRequest(conn, req); err != nil {
		return nil, fmt.Errorf("sandbox client: send request: %w", err)
	}
	// Arm the response budget HERE, from the instant the request went out.
	// Armed at dial instead, it would already have been spent by the greeting
	// and by anything else that happened first. Refs: MGIT-122
	_ = conn.SetReadDeadline(c.clock().Add(c.requestTimeout))
	resp, err := controlproto.ReadResponse(conn)
	if err != nil {
		return nil, fmt.Errorf("sandbox client: read response: %w", err)
	}
	return resp, nil
}

// SyncWorktree re-stages the task's host worktree into its running sandbox,
// or — with DryRun — asks only for the classification.
//
// A conflict comes back as an error AND a report: the error so a caller cannot
// mistake a refusal for a sync, the report so it can name every diverged path
// without asking again. Refs: MGIT-76, ADR-011
func (c *Client) SyncWorktree(ctx context.Context, taskID string, opts model.WorktreeSyncOptions) (*model.WorktreeSyncReport, error) {
	resp, err := c.roundTripRaw(ctx, &controlproto.Request{
		Kind: controlproto.KindSync, Sync: &controlproto.SyncArgs{TaskID: taskID, Sync: opts},
	})
	if err != nil {
		return nil, err
	}
	if resp.Error != "" {
		return resp.Synced, fmt.Errorf("sandbox: %s", resp.Error)
	}
	return resp.Synced, nil
}

// Launch registers a sandbox for a task and returns its info (lazy: the VM
// boots on first exec). Refs: FR-17.10
func (c *Client) Launch(ctx context.Context, opts model.SandboxLaunchOptions) (*model.SandboxInfo, error) {
	resp, err := c.roundTrip(ctx, &controlproto.Request{Kind: controlproto.KindLaunch, Launch: &opts})
	if err != nil {
		return nil, err
	}
	return resp.Sandbox, nil
}

// List returns every registered sandbox.
func (c *Client) List(ctx context.Context) ([]model.SandboxInfo, error) {
	resp, err := c.roundTrip(ctx, &controlproto.Request{Kind: controlproto.KindList})
	if err != nil {
		return nil, err
	}
	return resp.List, nil
}

// Status returns the sandbox bound to a task.
func (c *Client) Status(ctx context.Context, taskID string) (*model.SandboxInfo, error) {
	resp, err := c.roundTrip(ctx, &controlproto.Request{
		Kind: controlproto.KindStatus, Status: &controlproto.TaskRef{TaskID: taskID},
	})
	if err != nil {
		return nil, err
	}
	return resp.Sandbox, nil
}

// Remove tears down a task's sandbox.
func (c *Client) Remove(ctx context.Context, taskID string, force bool) error {
	_, err := c.roundTrip(ctx, &controlproto.Request{
		Kind: controlproto.KindRemove, Remove: &controlproto.RemoveArgs{TaskID: taskID, Force: force},
	})
	return err
}

// Land pulls the task's guest commit objects over the dedicated land
// channel, verifies them host-side, and atomically imports + fast-forwards
// the task branch. It returns the number of new commits landed and the
// branch advanced. The whole verified path runs in the daemon; the client
// only names the task. Refs: FR-17.5, MGIT-11.10.10
func (c *Client) Land(ctx context.Context, taskID string) (*controlproto.LandResult, error) {
	resp, err := c.roundTrip(ctx, &controlproto.Request{
		Kind: controlproto.KindLand, Land: &controlproto.TaskRef{TaskID: taskID},
	})
	if err != nil {
		return nil, err
	}
	return resp.Landed, nil
}

// Grants lists a task's pending capability requests awaiting operator approval
// (derived host-side from observed egress denials, SEC-05). Refs: FR-17.12
func (c *Client) Grants(ctx context.Context, taskID string) ([]controlproto.PendingGrant, error) {
	resp, err := c.roundTrip(ctx, &controlproto.Request{
		Kind: controlproto.KindGrants, Grants: &controlproto.TaskRef{TaskID: taskID},
	})
	if err != nil {
		return nil, err
	}
	return resp.Pending, nil
}

// Grant approves one pending capability request (by its host-observed key) for
// a task's sandbox, returning the granted destination. Refs: FR-17.12
func (c *Client) Grant(ctx context.Context, taskID, key string) (*controlproto.GrantResult, error) {
	resp, err := c.roundTrip(ctx, &controlproto.Request{
		Kind: controlproto.KindGrant, Grant: &controlproto.GrantArgs{TaskID: taskID, Key: key},
	})
	if err != nil {
		return nil, err
	}
	return resp.Granted, nil
}

// SetEgressPolicy replaces a RUNNING sandbox's egress allowlist without
// relaunching it. An EMPTY entries list is a full revoke.
//
// drain leaves ESTABLISHED flows to finish; the default terminates them,
// because a draining connection is exactly the exfiltration channel the caller
// just revoked (ADR-012). The reply states what was actually enforced and how
// many established flows were terminated — outcomes, not intentions.
// Refs: MGIT-72, SEC-04
func (c *Client) SetEgressPolicy(ctx context.Context, taskID string, entries []string, drain bool) (*controlproto.PolicyResult, error) {
	resp, err := c.roundTrip(ctx, &controlproto.Request{
		Kind:      controlproto.KindPolicySet,
		PolicySet: &controlproto.PolicyArgs{TaskID: taskID, Entries: entries, Drain: drain},
	})
	if err != nil {
		return nil, err
	}
	return resp.Policy, nil
}

// EgressPolicy reports the allowlist a task's RUNNING sandbox is enforcing
// right now, which after a live mutation is not its launch-time policy.
// Refs: MGIT-72
func (c *Client) EgressPolicy(ctx context.Context, taskID string) (*controlproto.PolicyResult, error) {
	resp, err := c.roundTrip(ctx, &controlproto.Request{
		Kind: controlproto.KindPolicyShow, PolicyShow: &controlproto.TaskRef{TaskID: taskID},
	})
	if err != nil {
		return nil, err
	}
	return resp.Policy, nil
}

// Exec runs one command in a task's sandbox, copying stdout/stderr to the
// supplied writers as frames arrive and returning the guest exit code. A
// supervisor-level failure (the guest could not start the command) is
// returned as an error with a -1 exit code.
//
// NO WALL CLOCK BOUNDS THE COMMAND, and that is the contract, not an omission.
// The daemon relays the command's output only once the command has finished, so
// a deadline measured over this whole read is a cap on how long a build, test
// run or install may take — a control-plane timeout deciding a workload's
// lifetime. It used to be exactly that: the deadline armed at dial ended every
// exec at 30 s, and the failure was rendered to the agent as in-guest memory
// exhaustion (MGIT-122, and the diagnosis defect MGIT-118).
//
// What IS bounded is SILENCE. The daemon beats on this stream while the exec is
// outstanding, so the gap between frames — not their span — is the deadline: a
// build that prints nothing for an hour keeps arriving beats and lives, while a
// daemon that stalls falls silent and is named as the suspect within seconds.
// A daemon that never beats at all is an older one, and is waited on unbounded
// rather than accused (see relayFrames).
//
// Also bounding the wait: the per-exec ExecRequest.Timeout and the sandbox TTL,
// which the daemon enforces guest-side; a dead daemon, whose socket closes and
// ends the read at once; and ctx, wired to the connection here so a caller that
// gives up is never left blocked. Refs: FR-17.11, FR-17.11.1, MGIT-122, MGIT-133
func (c *Client) Exec(ctx context.Context, taskID string, req model.ExecRequest, stdout, stderr io.Writer) (int, error) {
	conn, err := c.dialGreeted(ctx)
	if err != nil {
		return -1, err
	}
	defer func() { _ = conn.Close() }()
	defer watchCancel(ctx, conn)()

	_ = conn.SetWriteDeadline(c.clock().Add(c.requestTimeout))
	if err := controlproto.WriteRequest(conn, &controlproto.Request{
		Kind: controlproto.KindExec,
		Exec: &controlproto.ExecArgs{TaskID: taskID, Exec: req},
	}); err != nil {
		return -1, fmt.Errorf("sandbox client: send exec: %w", err)
	}
	return c.relayFrames(ctx, conn, stdout, stderr)
}

// Shell attaches an interactive session to a task's sandbox (T2
// fully-confined agent, MGIT-11.11.4). The host-side orchestration —
// per-session credential injection and audit flagging — is implemented in
// service.ConfinedSessionService; the bidirectional vsock-PTY transport
// that carries an interactive session to the guest is KVM-gated guest
// infrastructure not served by this daemon build. Rather than silently
// degrade to a non-interactive session (which would mislead a caller
// expecting a shell), Shell reports ErrShellTransportUnavailable.
// Refs: MGIT-11.11.4
func (c *Client) Shell(_ context.Context, _ string, _ io.Reader, _, _ io.Writer) (int, error) {
	return -1, fmt.Errorf("%w", model.ErrShellTransportUnavailable)
}

// relayFrames copies the daemon's exec frame stream to the writers and
// returns the exit code from the terminal result frame.
//
// It arms an IDLE deadline before every read: the longest silence a beating
// daemon can produce. Every frame — beat, output, result — rearms it, so the
// clock measures the gap between frames and never the command.
//
// A daemon that has NOT beaten by the time that deadline first fires is not
// accused of anything. It is an mgit-sandboxd older than MGIT-133, which emits
// no beats at all, or (far less likely) a current one that stalled inside the
// first window; either way there is no liveness signal to judge, so the
// deadline is dropped, MGIT-122's unbounded wait is restored, and the caller is
// told plainly which of those two facts it is looking at. Treating "never
// beat" as "wedged" would break every mixed-version pair — an upgraded CLI
// against the long-lived daemon the previous release left running is the
// ordinary case, not an exotic one. Refs: MGIT-133, MGIT-122
func (c *Client) relayFrames(ctx context.Context, conn net.Conn, stdout, stderr io.Writer) (int, error) {
	stall, beating := c.stallTimeout, false
	for {
		c.armStall(conn, stall)
		kind, payload, err := execwire.ReadFrame(conn)
		if err != nil {
			// Cancellation reaches this read AS an expired deadline —
			// watchCancel expires the connection rather than closing it — so
			// the caller's own withdrawal must be ruled out before the error
			// is read as anything about the daemon. Without this check a
			// Ctrl-C on a non-beating daemon was silently swallowed and the
			// loop waited forever. Refs: MGIT-122, MGIT-133
			if ctxErr := ctx.Err(); ctxErr != nil {
				return -1, fmt.Errorf("sandbox client: exec canceled: %w", ctxErr)
			}
			if !errors.Is(err, os.ErrDeadlineExceeded) {
				return -1, fmt.Errorf("sandbox client: read exec stream: %w", err)
			}
			if beating {
				return -1, stalledDaemonError(stall)
			}
			writeNoLivenessNotice(stderr, stall)
			stall = 0
			continue
		}
		switch kind {
		case execwire.FrameHeartbeat:
			// Proof of life, and — on the first one — proof that silence from
			// this daemon is meaningful. Carries no data by construction.
			beating = true
		case execwire.FrameStdout:
			if _, err := stdout.Write(payload); err != nil {
				return -1, fmt.Errorf("sandbox client: write stdout: %w", err)
			}
		case execwire.FrameStderr:
			if _, err := stderr.Write(payload); err != nil {
				return -1, fmt.Errorf("sandbox client: write stderr: %w", err)
			}
		case execwire.FrameResult:
			var rf execwire.ResultFrame
			if err := json.Unmarshal(payload, &rf); err != nil {
				return -1, fmt.Errorf("sandbox client: decode result: %w", err)
			}
			if rf.Error != "" {
				return -1, errors.New("sandbox exec: " + rf.Error)
			}
			return rf.Result.ExitCode, nil
		default:
			return -1, fmt.Errorf("sandbox client: unexpected exec frame %#x", kind)
		}
	}
}

// armStall sets the idle deadline for the next frame read, or clears it when
// the stream has been declared unjudgeable (stall == 0). Clearing is explicit
// so the absence of a deadline reads as a decision, not an oversight.
func (c *Client) armStall(conn net.Conn, stall time.Duration) {
	if stall <= 0 {
		_ = conn.SetReadDeadline(time.Time{})
		return
	}
	_ = conn.SetReadDeadline(c.clock().Add(stall))
}

// stalledDaemonError names the daemon as the suspect, and says why the command
// is not one.
//
// The wording is load-bearing. An exec that dies mid-flight is otherwise always
// read as a guest failure, and MGIT-118 is what that costs: an agent told its
// build had exhausted guest memory spent the next hour shrinking a build that
// was never the problem. A stall here is a statement about the daemon, so the
// message says so first, gives the reader the reasoning rather than an
// assertion, and points at a check that distinguishes the two.
// Refs: MGIT-133, MGIT-118
func stalledDaemonError(stall time.Duration) error {
	return fmt.Errorf("%w: no liveness beat for %s while the command was still running.\n"+
		"  The DAEMON is the suspect — not your command, and not the guest. mgit-sandboxd beats\n"+
		"  every %s on an open exec stream however long the command takes, so silence means the\n"+
		"  daemon stopped answering, NOT that the build is slow and NOT that the guest ran out of\n"+
		"  memory. Your command may still be running inside the guest.\n"+
		"  Check with `mgit sandbox list`: if that hangs too, the daemon is wedged",
		model.ErrSandboxDaemonUnresponsive, stall, execwire.HeartbeatInterval)
}

// writeNoLivenessNotice tells the caller, once, that this daemon offers no
// liveness signal — so a wait that goes on forever is understood as "nothing
// here can tell you whether it is stuck", not as a promise that it is fine.
// Refs: MGIT-133
func writeNoLivenessNotice(w io.Writer, stall time.Duration) {
	_, _ = fmt.Fprintf(w, "mgit: this mgit-sandboxd sends no liveness beats (it predates MGIT-133), "+
		"so after %s of silence mgit cannot tell a working command from a stalled daemon. "+
		"Waiting without a stall check; restart the daemon from a current install to get one.\n", stall)
}

// ExportArtifact copies a guest-built path out of a task's sandbox to a
// host-named destination, returning what crossed the boundary.
//
// Both paths travel host-side only: the client names them, the daemon resolves
// the task to its sandbox, and the backend reads the staged worktree
// host-side — the guest is never asked and never participates.
// Refs: MGIT-73, ADR-011
func (c *Client) ExportArtifact(ctx context.Context, taskID string,
	req model.ArtifactExportRequest) (*model.ArtifactExportResult, error) {
	resp, err := c.roundTrip(ctx, &controlproto.Request{
		Kind: controlproto.KindExport, Export: &controlproto.ExportArgs{TaskID: taskID, Export: req},
	})
	if err != nil {
		return nil, err
	}
	return resp.Exported, nil
}
