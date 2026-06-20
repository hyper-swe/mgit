package sandboxd

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"

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
	if d.cfg.Service == nil {
		return
	}
	_ = conn.SetReadDeadline(d.cfg.Clock().Add(controlproto.DefaultRequestTimeout))
	req, err := controlproto.ReadRequest(conn)
	if err != nil {
		// A clean EOF is the benign greeting-probe close (activation health
		// check). Anything else is a malformed/oversized/slow client: fail
		// closed, audit the rejection, reply best-effort.
		if !errors.Is(err, io.EOF) {
			d.cfg.Logger.Warn("sandboxd rejected request",
				"event", "request_rejected", "error", err.Error())
			d.writeResponse(conn, &controlproto.Response{Error: "invalid request"})
		}
		return
	}
	d.dispatch(ctx, conn, req)
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
	default:
		// KindLand is a valid protocol kind but is served by the land
		// orchestrator (MGIT-11.10.10), not this dispatcher.
		d.reply(conn, &controlproto.Response{},
			fmt.Errorf("controlproto kind %#x not served by this daemon", req.Kind))
	}
}

// reply writes a success response, or an error response carrying a
// host-observed message (SEC-05: never guest-sourced text). A failed
// operation is audited operationally. Refs: MGIT-11.10.8
func (d *Daemon) reply(conn net.Conn, resp *controlproto.Response, opErr error) {
	if opErr != nil {
		d.cfg.Logger.Warn("sandboxd op failed", "event", "op_error", "error", opErr.Error())
		resp = &controlproto.Response{Error: opErr.Error()}
	}
	d.writeResponse(conn, resp)
}

// armWriteDeadline bounds a single reply write so a stalled client cannot
// wedge a handler goroutine indefinitely.
func (d *Daemon) armWriteDeadline(conn net.Conn) {
	_ = conn.SetWriteDeadline(d.cfg.Clock().Add(controlproto.DefaultRequestTimeout))
}

// writeResponse sends one control response under a write deadline.
func (d *Daemon) writeResponse(conn net.Conn, resp *controlproto.Response) {
	d.armWriteDeadline(conn)
	if err := controlproto.WriteResponse(conn, resp); err != nil {
		d.cfg.Logger.Warn("sandboxd write response failed",
			"event", "write_error", "error", err.Error())
	}
}

// serveExec runs one command through the service and relays the outcome as
// an execwire frame stream: stdout ('O') and stderr ('E') in bounded
// chunks, then the terminal result ('R') carrying the exit code. A setup
// failure is reported as a result frame with an error string so the
// client's frame reader always sees a clean end-of-stream.
// Refs: FR-17.11, MGIT-11.10.8
func (d *Daemon) serveExec(ctx context.Context, conn net.Conn, args *controlproto.ExecArgs) {
	res, err := d.cfg.Service.Exec(ctx, args.TaskID, args.Exec)
	if err != nil {
		d.cfg.Logger.Warn("sandboxd exec failed", "event", "op_error", "error", err.Error())
		d.writeResultFrame(conn, execwire.Result{}, err.Error())
		return
	}
	d.armWriteDeadline(conn)
	if !d.relayChunks(conn, execwire.FrameStdout, res.Stdout) ||
		!d.relayChunks(conn, execwire.FrameStderr, res.Stderr) {
		return // the connection is gone; the result frame would also fail
	}
	d.writeResultFrame(conn, execwire.Result{ExitCode: res.ExitCode}, "")
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
