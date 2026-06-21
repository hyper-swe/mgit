package egress

import (
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/netip"
	"sync"
	"time"
)

// maxConnectRequestBytes bounds one CONNECT request frame. A DNS name maxes
// at 253 bytes; this leaves room for the JSON envelope while refusing a
// hostile guest's attempt to force unbounded host-side allocation
// (land-path hardening class). Refs: SEC-04
const maxConnectRequestBytes = 512

const (
	statusDeny  byte = 0
	statusAllow byte = 1
)

// ConnectRequest is the guest's egress ask: connect to Host:Port over
// Protocol. The host authorizes it on host-observed facts and never trusts
// these fields beyond driving host-side resolution. Shared by the proxy and
// the in-guest egress client so the wire format has one definition.
// Refs: SEC-04, FR-17.8
type ConnectRequest struct {
	Protocol string `json:"protocol"`
	Host     string `json:"host"`
	Port     int    `json:"port"`
}

// DialFunc opens a host-side TCP connection to an authorized destination IP
// and port. Injected so the proxy is testable without real sockets and so
// the bound external interface is host-controlled. Refs: SEC-04
type DialFunc func(ctx context.Context, ip netip.Addr, port int) (net.Conn, error)

// ProxyConfig wires the egress proxy.
type ProxyConfig struct {
	Authorizer *Authorizer
	Dial       DialFunc
	Logger     *slog.Logger
	// HandshakeTimeout bounds reading the CONNECT frame from a possibly
	// hung/hostile guest (0 => 30s). It does not bound the spliced flow.
	HandshakeTimeout time.Duration
}

// Proxy is the host egress proxy: it terminates the guest's only egress
// path, authorizes each CONNECT against the policy, and (on allow) splices
// to a host-dialed connection to the pinned destination IP. The guest has
// no direct route, so this is the sole way out — a hostile guest cannot
// bypass it. Refs: SEC-04, FR-17.8
type Proxy struct {
	cfg ProxyConfig
}

// NewProxy validates the configuration and returns a Proxy.
func NewProxy(cfg ProxyConfig) (*Proxy, error) {
	switch {
	case cfg.Authorizer == nil:
		return nil, fmt.Errorf("egress proxy: authorizer must not be nil")
	case cfg.Dial == nil:
		return nil, fmt.Errorf("egress proxy: dialer must not be nil")
	case cfg.Logger == nil:
		return nil, fmt.Errorf("egress proxy: logger must not be nil")
	}
	if cfg.HandshakeTimeout <= 0 {
		cfg.HandshakeTimeout = 30 * time.Second
	}
	return &Proxy{cfg: cfg}, nil
}

// Serve accepts guest connections until the listener closes or ctx is
// canceled, handling each in its own goroutine. A listener-closed error is
// a clean shutdown, not a failure. Refs: SEC-04
func (p *Proxy) Serve(ctx context.Context, ln net.Listener) error {
	for {
		conn, err := ln.Accept()
		if err != nil {
			if errors.Is(err, net.ErrClosed) || ctx.Err() != nil {
				return nil //nolint:nilerr // listener-closed / ctx-cancel is a clean shutdown, not a failure
			}
			return fmt.Errorf("egress proxy accept: %w", err)
		}
		go p.handle(ctx, conn)
	}
}

// handle reads one CONNECT request, authorizes it, and either splices to a
// host-dialed upstream (allow) or returns a deny reply (deny). The request
// read is deadline-bounded; the splice is not. Refs: SEC-04, FR-17.8
func (p *Proxy) handle(ctx context.Context, guest net.Conn) {
	defer func() { _ = guest.Close() }()

	_ = guest.SetReadDeadline(time.Now().Add(p.cfg.HandshakeTimeout))
	req, err := DecodeConnectRequest(guest)
	if err != nil {
		// A malformed/oversized header gets no reply — close and move on.
		p.cfg.Logger.Warn("egress proxy: bad connect frame", "event", "egress_badframe", "error", err.Error())
		return
	}
	_ = guest.SetReadDeadline(time.Time{}) // clear; the splice runs unbounded

	// ConnectRequest and Flow carry the same (protocol, host, port) shape;
	// the conversion keeps the wire type and the policy type distinct.
	decision, err := p.cfg.Authorizer.Authorize(ctx, Flow(req))
	if err != nil {
		_ = encodeReply(guest, false, decision.Rule)
		return
	}

	upstream, err := p.cfg.Dial(ctx, decision.DestIP, req.Port)
	if err != nil {
		p.cfg.Logger.Warn("egress proxy: upstream dial failed", "event", "egress_dialfail",
			"dest_ip", decision.DestIP.String(), "error", err.Error())
		_ = encodeReply(guest, false, "upstream dial failed")
		return
	}
	defer func() { _ = upstream.Close() }()

	if err := encodeReply(guest, true, ""); err != nil {
		return
	}
	splice(guest, upstream)
}

// splice copies bytes both ways until either side closes, then unblocks the
// other by closing both. Refs: SEC-04
func splice(a, b net.Conn) {
	var wg sync.WaitGroup
	wg.Add(2)
	cp := func(dst, src net.Conn) {
		defer wg.Done()
		_, _ = io.Copy(dst, src)
		_ = a.Close()
		_ = b.Close()
	}
	go cp(a, b)
	go cp(b, a)
	wg.Wait()
}

// EncodeConnectRequest writes a length-prefixed CONNECT request. Shared with
// the in-guest egress client. Refs: SEC-04
func EncodeConnectRequest(w io.Writer, req ConnectRequest) error {
	payload, err := json.Marshal(req)
	if err != nil {
		return fmt.Errorf("encode connect request: %w", err)
	}
	if len(payload) > maxConnectRequestBytes {
		return fmt.Errorf("connect request exceeds %d bytes", maxConnectRequestBytes)
	}
	var hdr [2]byte
	binary.BigEndian.PutUint16(hdr[:], uint16(len(payload)))
	if _, err := w.Write(hdr[:]); err != nil {
		return fmt.Errorf("write connect header: %w", err)
	}
	if _, err := w.Write(payload); err != nil {
		return fmt.Errorf("write connect payload: %w", err)
	}
	return nil
}

// DecodeConnectRequest reads a length-prefixed CONNECT request, enforcing
// the frame ceiling before allocating. Refs: SEC-04
func DecodeConnectRequest(r io.Reader) (ConnectRequest, error) {
	var hdr [2]byte
	if _, err := io.ReadFull(r, hdr[:]); err != nil {
		return ConnectRequest{}, fmt.Errorf("read connect header: %w", err)
	}
	n := binary.BigEndian.Uint16(hdr[:])
	if n == 0 || int(n) > maxConnectRequestBytes {
		return ConnectRequest{}, fmt.Errorf("connect frame size %d out of range (1..%d)", n, maxConnectRequestBytes)
	}
	payload := make([]byte, n)
	if _, err := io.ReadFull(r, payload); err != nil {
		return ConnectRequest{}, fmt.Errorf("read connect payload: %w", err)
	}
	var req ConnectRequest
	if err := json.Unmarshal(payload, &req); err != nil {
		return ConnectRequest{}, fmt.Errorf("decode connect payload: %w", err)
	}
	return req, nil
}

// encodeReply writes the 1-byte status + length-prefixed reason.
func encodeReply(w io.Writer, allow bool, reason string) error {
	status := statusDeny
	if allow {
		status = statusAllow
	}
	if len(reason) > maxConnectRequestBytes {
		reason = reason[:maxConnectRequestBytes]
	}
	var buf [3]byte
	buf[0] = status
	binary.BigEndian.PutUint16(buf[1:], uint16(len(reason)))
	if _, err := w.Write(buf[:]); err != nil {
		return fmt.Errorf("write reply header: %w", err)
	}
	// Skip a zero-length reason write: on a synchronous transport (net.Pipe,
	// vsock) an empty write would block waiting for a read that ReadFull of
	// zero bytes never performs.
	if len(reason) == 0 {
		return nil
	}
	if _, err := io.WriteString(w, reason); err != nil {
		return fmt.Errorf("write reply reason: %w", err)
	}
	return nil
}

// DecodeConnectReply reads the proxy's allow/deny reply. Shared with the
// in-guest egress client so a denied flow surfaces the host reason. Refs: SEC-04
func DecodeConnectReply(r io.Reader) (bool, string, error) {
	var buf [3]byte
	if _, err := io.ReadFull(r, buf[:]); err != nil {
		return false, "", fmt.Errorf("read reply header: %w", err)
	}
	n := binary.BigEndian.Uint16(buf[1:])
	if int(n) > maxConnectRequestBytes {
		return false, "", fmt.Errorf("reply reason size %d exceeds %d", n, maxConnectRequestBytes)
	}
	reason := make([]byte, n)
	if _, err := io.ReadFull(r, reason); err != nil {
		return false, "", fmt.Errorf("read reply reason: %w", err)
	}
	return buf[0] == statusAllow, string(reason), nil
}
