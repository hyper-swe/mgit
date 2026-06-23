package sandboxd

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
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
}

// NewClient returns a control-plane client for the daemon at socketPath.
func NewClient(socketPath string, clock func() time.Time) *Client {
	return &Client{socketPath: socketPath, clock: clock}
}

// dialGreeted dials the daemon and consumes its liveness greeting,
// returning a connection ready to carry one request. A socket that does
// not greet is rejected (squatter defense). The caller closes the conn.
func (c *Client) dialGreeted(ctx context.Context) (net.Conn, error) {
	var dialer net.Dialer
	conn, err := dialer.DialContext(ctx, "unix", c.socketPath)
	if err != nil {
		return nil, fmt.Errorf("sandbox client: dial daemon: %w", err)
	}
	_ = conn.SetReadDeadline(c.clock().Add(controlproto.DefaultRequestTimeout))
	buf := make([]byte, len(greeting))
	if _, err := io.ReadFull(conn, buf); err != nil || string(buf) != greeting {
		_ = conn.Close()
		return nil, fmt.Errorf("sandbox client: daemon did not greet (not running, or a squatter holds the socket)")
	}
	return conn, nil
}

// roundTrip sends one request and returns the daemon's response, mapping a
// response-level error to a Go error. Used by the non-streaming verbs.
func (c *Client) roundTrip(ctx context.Context, req *controlproto.Request) (*controlproto.Response, error) {
	conn, err := c.dialGreeted(ctx)
	if err != nil {
		return nil, err
	}
	defer func() { _ = conn.Close() }()

	_ = conn.SetWriteDeadline(c.clock().Add(controlproto.DefaultRequestTimeout))
	if err := controlproto.WriteRequest(conn, req); err != nil {
		return nil, fmt.Errorf("sandbox client: send request: %w", err)
	}
	resp, err := controlproto.ReadResponse(conn)
	if err != nil {
		return nil, fmt.Errorf("sandbox client: read response: %w", err)
	}
	if resp.Error != "" {
		return nil, fmt.Errorf("sandbox: %s", resp.Error)
	}
	return resp, nil
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

// Exec runs one command in a task's sandbox, copying stdout/stderr to the
// supplied writers as frames arrive and returning the guest exit code. A
// supervisor-level failure (the guest could not start the command) is
// returned as an error with a -1 exit code. Refs: FR-17.11
func (c *Client) Exec(ctx context.Context, taskID string, req model.ExecRequest, stdout, stderr io.Writer) (int, error) {
	conn, err := c.dialGreeted(ctx)
	if err != nil {
		return -1, err
	}
	defer func() { _ = conn.Close() }()

	_ = conn.SetWriteDeadline(c.clock().Add(controlproto.DefaultRequestTimeout))
	if err := controlproto.WriteRequest(conn, &controlproto.Request{
		Kind: controlproto.KindExec,
		Exec: &controlproto.ExecArgs{TaskID: taskID, Exec: req},
	}); err != nil {
		return -1, fmt.Errorf("sandbox client: send exec: %w", err)
	}
	return relayFrames(conn, stdout, stderr)
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
func relayFrames(conn net.Conn, stdout, stderr io.Writer) (int, error) {
	for {
		kind, payload, err := execwire.ReadFrame(conn)
		if err != nil {
			return -1, fmt.Errorf("sandbox client: read exec stream: %w", err)
		}
		switch kind {
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
