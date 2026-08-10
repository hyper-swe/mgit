// Package vmctl is the HOST→VM-CHILD control channel: how the daemon acts on
// a running sandbox whose enforcement state lives in another process.
//
// WHY IT EXISTS. libkrun's krun_start_enter never returns and consumes the
// calling process, so every VM runs in a re-exec'd child (ADR-010, MGIT-61.8).
// The netstack egress gateway and its authorizer live in that child — which is
// correct, because the gateway's lifetime must match the VM's exactly — but it
// leaves the DAEMON, which owns the CLI/MCP surface and the task bindings,
// with no route to the thing actually enforcing policy. This channel is that
// route. Refs: MGIT-74, MGIT-72, ADR-010, ADR-011
//
// SHAPE. It is deliberately a general "act on a running sandbox" channel
// rather than a revoke-specific one: the same hop is what any future verb
// needs, and a single-purpose channel would have to be rebuilt.
//
// DIRECTION IS PART OF THE SECURITY MODEL. Host-initiated ONLY: the daemon
// dials, the child answers, and the child never pushes state the daemon then
// trusts. The socket is a per-VM host-side artifact under the sandbox state
// dir — a directory the guest never sees, because the guest's share is the
// staging subdirectory, not the state dir itself. The guest therefore cannot
// reach this channel at all, which is what keeps SEC-05 true after adding a
// way to mutate policy at runtime.
package vmctl

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"time"
)

// SocketName is the per-VM control socket, kept SHORT on purpose: every byte
// comes out of the 104-byte sun_path budget the whole state-dir path shares,
// a limit this backend has already hit once. Refs: MGIT-61.15, MGIT-74
const SocketName = "c.sock"

// Op names one control operation. New verbs are added here rather than by
// growing a second channel.
type Op string

const (
	// OpSetPolicy replaces a running sandbox's egress allowlist (MGIT-72).
	OpSetPolicy Op = "set-policy"
	// OpGetPolicy reports the allowlist a running sandbox is enforcing right
	// now. A mutable policy that cannot be READ is one a caller has to take on
	// faith, because the launch-time policy stops being true the moment it is
	// mutated. Refs: MGIT-72
	OpGetPolicy Op = "get-policy"
)

// Request is one host→child command. One request per connection, matching the
// exec channel's convention.
type Request struct {
	Op Op `json:"op"`
	// Entries is the replacement allowlist for OpSetPolicy.
	Entries []string `json:"entries,omitempty"`
	// Drain, for OpSetPolicy, leaves established flows to finish instead of
	// killing them. Opt-in by design (ADR-011).
	Drain bool `json:"drain,omitempty"`
}

// Response is the child's reply. OK=false with Error set is a REPORTED
// failure, which the host surfaces — never a silent success.
type Response struct {
	OK    bool   `json:"ok"`
	Error string `json:"error,omitempty"`
	// Killed counts established flows terminated by a policy change.
	Killed int `json:"killed,omitempty"`
	// Rules is how many rules the new policy compiled to.
	Rules int `json:"rules,omitempty"`
	// Drained reports that established flows were left to finish.
	Drained bool `json:"drained,omitempty"`
	// Entries is the allowlist now in force (get-policy, and echoed by
	// set-policy), so the caller sees the resulting state and not only the
	// state it asked for.
	Entries []string `json:"entries,omitempty"`
}

// dialTimeout bounds a control round trip. The child answers from memory, so
// anything slower than this is a wedged or absent child, not a slow one.
const dialTimeout = 5 * time.Second

// Handler is the child-side implementation of the control operations. It is an
// interface so the wire and the enforcement stay separable and each is
// testable without the other.
type Handler interface {
	// SetPolicy replaces the running allowlist, killing established flows
	// unless drain is set, and reports what it did.
	SetPolicy(entries []string, drain bool) (Response, error)
	// GetPolicy reports the allowlist currently in force.
	GetPolicy() (Response, error)
}

// Serve accepts control connections until the listener closes, handling one
// request per connection.
//
// It runs in the VM child alongside krun_start_enter, which holds the process
// but does not stop its goroutines — the same property that lets the netstack
// gateway run there. Refs: ADR-010, MGIT-74
func Serve(ln net.Listener, h Handler) error {
	for {
		conn, err := ln.Accept()
		if err != nil {
			return err // the listener closed at teardown
		}
		go serveConn(conn, h)
	}
}

// serveConn handles one request and closes.
func serveConn(conn net.Conn, h Handler) {
	defer func() { _ = conn.Close() }()
	_ = conn.SetDeadline(time.Now().Add(dialTimeout))

	var req Request
	if err := json.NewDecoder(io.LimitReader(conn, maxRequestBytes)).Decode(&req); err != nil {
		writeResponse(conn, Response{Error: fmt.Sprintf("decode request: %v", err)})
		return
	}
	writeResponse(conn, dispatch(req, h))
}

// maxRequestBytes bounds one control request. An allowlist is a short list of
// host:port entries; anything larger is malformed or hostile.
const maxRequestBytes = 64 << 10

// dispatch routes one request, refusing an unknown op rather than ignoring it
// — a silently dropped verb would look like success to the host.
func dispatch(req Request, h Handler) Response {
	switch req.Op {
	case OpSetPolicy:
		resp, err := h.SetPolicy(req.Entries, req.Drain)
		if err != nil {
			return Response{Error: err.Error()}
		}
		resp.OK = true
		return resp
	case OpGetPolicy:
		resp, err := h.GetPolicy()
		if err != nil {
			return Response{Error: err.Error()}
		}
		resp.OK = true
		return resp
	default:
		return Response{Error: fmt.Sprintf("unknown control op %q", req.Op)}
	}
}

func writeResponse(w io.Writer, resp Response) {
	_ = json.NewEncoder(w).Encode(resp)
}

// Client is the host side of the channel.
type Client struct{ SocketPath string }

// SetPolicy asks a running VM child to replace its egress allowlist.
//
// It FAILS CLOSED: an absent or unreachable socket returns an actionable
// error rather than reporting success. A revoke that claims to have worked
// while the VM keeps enforcing the old policy is worse than one that errors,
// because the caller then runs untrusted code believing egress is closed.
// Refs: MGIT-74, MGIT-72
func (c Client) SetPolicy(entries []string, drain bool) (Response, error) {
	return c.do(Request{Op: OpSetPolicy, Entries: entries, Drain: drain})
}

// GetPolicy asks a running VM child what egress allowlist it is enforcing.
//
// It FAILS CLOSED like SetPolicy: an unreachable child is an error, never an
// empty policy — an empty list would read as "nothing is allowed" when the
// truth may be "nothing is enforcing", which are opposite facts.
// Refs: MGIT-72
func (c Client) GetPolicy() (Response, error) {
	return c.do(Request{Op: OpGetPolicy})
}

// do performs one request/response round trip.
func (c Client) do(req Request) (Response, error) {
	ctx, cancel := context.WithTimeout(context.Background(), dialTimeout)
	defer cancel()
	var d net.Dialer
	conn, err := d.DialContext(ctx, "unix", c.SocketPath)
	if err != nil {
		return Response{}, fmt.Errorf(
			"vm control channel unreachable at %s: %w "+
				"(the sandbox may not be running, or its VM predates this capability); "+
				"the running policy was NOT changed",
			c.SocketPath, err)
	}
	defer func() { _ = conn.Close() }()
	_ = conn.SetDeadline(time.Now().Add(dialTimeout))

	if err := json.NewEncoder(conn).Encode(req); err != nil {
		return Response{}, fmt.Errorf("vm control channel: send %s: %w", req.Op, err)
	}
	var resp Response
	if err := json.NewDecoder(io.LimitReader(conn, maxRequestBytes)).Decode(&resp); err != nil {
		return Response{}, fmt.Errorf("vm control channel: read reply to %s: %w", req.Op, err)
	}
	if !resp.OK {
		return resp, fmt.Errorf("vm control channel: %s refused: %s", req.Op, resp.Error)
	}
	return resp, nil
}
