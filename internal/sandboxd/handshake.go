package sandboxd

import (
	"context"
	"encoding/json"
	"fmt"
	"net"

	"github.com/hyper-swe/mgit/internal/controlproto"
	"github.com/hyper-swe/mgit/internal/execwire"
	"github.com/hyper-swe/mgit/internal/model"
)

// THE VERSION HANDSHAKE, both halves, in one file.
//
// The rule itself and the reason this control plane needs one live in
// internal/controlproto/handshake.go. What is decided HERE is the sequencing,
// and it is decided by one measured fact: after the greeting, an mgit 0.5.x
// client's next read depends on the verb it is about to send — a
// length-prefixed control response for every verb except exec, and an execwire
// frame stream for exec. No byte sequence written before that client's request
// can be legible to both, which is why the greeting line is left byte-identical
// and the version is exchanged in the frame that follows it.
//
// Sequence, on every connection:
//
//	daemon  -> "ok mgit-sandboxd\n"          (unchanged; squatter + activation probe)
//	client  -> KindHello{protocol, version}
//	daemon  -> Response{Hello{protocol, version}} (+ Error when they differ)
//	client  -> one verb                       (only if the versions match)
//
// A peer that sends a verb where the hello belongs is a build from before this
// existed. It is refused in the shape that verb's client is waiting for, which
// is what lets an mgit 0.5.x user read a real explanation instead of
// "unexpected exec frame 0x48" followed by a memory-cap advisory (MGIT-136).
//
// Refs: MGIT-136, MGIT-133, MGIT-118, FR-17.34

// self describes this daemon for a mismatch message.
func (d *Daemon) self() controlproto.Peer {
	return controlproto.Peer{Protocol: controlproto.ProtocolVersion, Version: d.cfg.Version}
}

// serveHandshake performs the version exchange and then, only for a matching
// peer, serves exactly one verb.
//
// A greet-only build (no service wired) serves nothing at all, handshake
// included: it exists to answer the activation liveness probe, and answering a
// version question it cannot then act on would be a claim it cannot keep.
// Refs: MGIT-136, FR-17.34
func (d *Daemon) serveHandshake(ctx context.Context, conn net.Conn) {
	if d.cfg.Service == nil {
		return
	}
	req, ok := d.readRequest(conn)
	if !ok {
		return
	}
	if req.Kind != controlproto.KindHello {
		d.refuseUnversionedPeer(conn, req.Kind)
		return
	}
	peer := controlproto.Peer{Protocol: req.Hello.Protocol, Version: req.Hello.Version}
	resp := &controlproto.Response{Hello: &controlproto.HelloResult{
		Protocol: controlproto.ProtocolVersion, Version: d.cfg.Version,
	}}
	if !controlproto.Compatible(peer.Protocol) {
		resp.Error = controlproto.SkewMessage(peer, d.self())
		d.auditSkew(peer.Protocol, "hello")
		d.writeResponse(conn, resp)
		return // nothing is transacted with an incompatible peer
	}
	d.writeResponse(conn, resp)
	d.serveRequest(ctx, conn)
}

// refuseUnversionedPeer answers a peer that sent a verb where its version
// belonged. Such a peer cannot state a build, so the message says what IS
// known — that it speaks the pre-handshake protocol — rather than inventing a
// version for it.
//
// The refusal is shaped by the verb, because that is what the peer is waiting
// to read. Refs: MGIT-136
func (d *Daemon) refuseUnversionedPeer(conn net.Conn, kind byte) {
	d.auditSkew(controlproto.LegacyProtocol, string(kind))
	msg := controlproto.SkewMessage(controlproto.Peer{Protocol: controlproto.LegacyProtocol}, d.self())
	if kind == controlproto.KindExec {
		d.refuseExecUnversioned(conn, msg)
		return
	}
	d.writeResponse(conn, &controlproto.Response{Error: msg})
}

// refuseExecUnversioned answers an unversioned exec in execwire frames: the
// whole message on stderr, where the client copies it straight to the
// operator's terminal, then a terminal result frame so the client's frame
// reader sees a clean end-of-stream instead of a truncated one.
//
// A response-shaped refusal here is exactly the MGIT-136 defect: the old
// client reads the first byte of the response length as a frame tag, calls it
// an unexpected frame, and its classifier reports the whole thing as a guest
// lost mid-command with a memory-cap advisory attached. Refs: MGIT-136, MGIT-118
func (d *Daemon) refuseExecUnversioned(conn net.Conn, msg string) {
	d.armWriteDeadline(conn)
	if err := execwire.WriteFrame(conn, execwire.FrameStderr, []byte(msg+"\n")); err != nil {
		return // the peer is gone; there is no one to explain this to
	}
	payload, err := json.Marshal(execRefusalFrame())
	if err != nil {
		d.cfg.Logger.Warn("sandboxd encode refusal frame failed",
			"event", "write_error", "error", err.Error())
		return
	}
	d.armWriteDeadline(conn)
	if err := execwire.WriteFrame(conn, execwire.FrameResult, payload); err != nil {
		d.cfg.Logger.Warn("sandboxd write refusal frame failed",
			"event", "write_error", "error", err.Error())
	}
}

// execRefusalFrame builds the terminal frame of a refused exec.
//
// Its Error names the skew (so the failure is never anonymous) and states that
// nothing ran, which is the fact the caller most needs: the guest was never
// asked, so nothing about it — least of all its memory cap — is implicated.
// The full remedy went out on the stderr frame just before this one rather
// than in here, because a multi-line remedy inside an error string is rendered
// by the old client in the middle of a sentence. Refs: MGIT-136
func execRefusalFrame() execwire.ResultFrame {
	return execwire.ResultFrame{
		Result: execwire.Result{ExitCode: -1},
		Error: model.ErrSandboxVersionSkew.Error() +
			" — nothing was run in the guest; see the upgrade instructions above",
	}
}

// auditSkew records a refused pair. An operator diagnosing "mgit suddenly
// refuses everything" should find the reason in the daemon's own log, not only
// in the CLI's output. Refs: MGIT-136
func (d *Daemon) auditSkew(peerProtocol int, where string) {
	d.cfg.Logger.Warn("sandboxd refused an incompatible client",
		"event", "version_skew",
		"peer_protocol", peerProtocol,
		"daemon_protocol", controlproto.ProtocolVersion,
		"daemon_version", d.cfg.Version,
		"at", where)
}

// --- client half ------------------------------------------------------------

// versionSkewError carries the full remedy text while staying comparable to
// the sentinel, so callers use errors.Is rather than matching on prose — the
// contract MGIT-109 established for this control plane. Refs: MGIT-136, MGIT-109
type versionSkewError struct{ msg string }

func (e *versionSkewError) Error() string { return e.msg }

// Unwrap makes every skew failure errors.Is(err, model.ErrSandboxVersionSkew),
// which is what the CLI's failure classifier keys off to settle a mismatch
// AHEAD of any guest-shaped conclusion. Refs: MGIT-136, MGIT-118
func (e *versionSkewError) Unwrap() error { return model.ErrSandboxVersionSkew }

// newSkewError renders the mismatch between this client and a daemon.
func newSkewError(clientVersion string, daemon controlproto.Peer) error {
	return &versionSkewError{msg: controlproto.SkewMessage(
		controlproto.Peer{Protocol: controlproto.ProtocolVersion, Version: clientVersion}, daemon)}
}

// handshake states this build's wire version and reads the daemon's, refusing
// the connection when they differ.
//
// WHAT IS AND IS NOT EVIDENCE OF A VERSION. A transport failure — a closed
// socket, a timeout, an undecodable reply — says nothing about the peer's
// version, and is returned as itself. Reading it as skew would make every
// dropped connection look like an upgrade problem and send the operator to fix
// the wrong thing. Only a daemon that ANSWERED is judged: an answer with no
// hello in it comes from a build that does not know the verb, which is by
// definition pre-handshake, and an answer with a different protocol number
// says so directly. Refs: MGIT-136
func (c *Client) handshake(conn net.Conn) error {
	_ = conn.SetWriteDeadline(c.clock().Add(c.requestTimeout))
	if err := controlproto.WriteRequest(conn, &controlproto.Request{
		Kind: controlproto.KindHello,
		Hello: &controlproto.HelloArgs{
			Protocol: controlproto.ProtocolVersion, Version: c.version,
		},
	}); err != nil {
		return fmt.Errorf("sandbox client: send version handshake: %w", err)
	}
	_ = conn.SetReadDeadline(c.clock().Add(c.requestTimeout))
	resp, err := controlproto.ReadResponse(conn)
	if err != nil {
		return fmt.Errorf("sandbox client: read version handshake: %w", err)
	}
	if resp.Hello == nil {
		// It greeted, it answered, and it does not know this verb: every mgit
		// up to 0.5.x replies "invalid request" to an unknown kind.
		return newSkewError(c.version, controlproto.Peer{Protocol: controlproto.LegacyProtocol})
	}
	if !controlproto.Compatible(resp.Hello.Protocol) {
		return newSkewError(c.version,
			controlproto.Peer{Protocol: resp.Hello.Protocol, Version: resp.Hello.Version})
	}
	return nil
}
