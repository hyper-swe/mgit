// Package fcvsock dials a firecracker guest's vsock port from the host.
// Firecracker exposes guest vsock over a per-VM unix socket using
// host-initiated connections (the firecracker "uds" model): the host
// connects to the unix socket, sends "CONNECT <port>\n", and the
// firecracker VMM replies "OK <hostport>\n" before proxying the stream to
// the guest's vsock port. The handshake is plain text over the unix
// socket, so this transport is OS-portable and unit-testable without a VM
// or /dev/kvm; only the per-sandbox socket path is firecracker-specific.
// It is the concrete realization of the microvm.GuestDialer transport for
// the firecracker backend. Refs: FR-17.11, MGIT-11.5
package fcvsock

import (
	"context"
	"fmt"
	"io"
	"net"
	"strings"
)

// maxHandshakeBytes bounds the firecracker reply line so a garbled or
// truncated handshake fails fast instead of reading unbounded. The real
// reply ("OK <port>\n") is a few bytes; this is generous headroom.
const maxHandshakeBytes = 128

// Dial connects to the firecracker vsock unix socket at udsPath and
// performs the CONNECT handshake to reach the guest's vsock port,
// returning the proxied stream. On any handshake failure the underlying
// connection is closed. The caller owns the returned conn. Refs: FR-17.11
func Dial(ctx context.Context, udsPath string, guestPort uint32) (net.Conn, error) {
	var d net.Dialer
	conn, err := d.DialContext(ctx, "unix", udsPath)
	if err != nil {
		return nil, fmt.Errorf("fcvsock: dial %q: %w", udsPath, err)
	}
	if err := connectHandshake(conn, guestPort); err != nil {
		_ = conn.Close()
		return nil, err
	}
	return conn, nil
}

// connectHandshake performs the firecracker host-initiated CONNECT
// handshake on an open stream: write "CONNECT <port>\n", then expect a
// reply line beginning with "OK ". Any other reply (or a read/write
// error) is a failure. The reply is read one byte at a time up to the
// newline so no guest stream bytes that follow are consumed — the caller
// reads the proxied stream cleanly from the same conn afterwards.
func connectHandshake(rw io.ReadWriter, guestPort uint32) error {
	if _, err := fmt.Fprintf(rw, "CONNECT %d\n", guestPort); err != nil {
		return fmt.Errorf("fcvsock: send CONNECT: %w", err)
	}
	line, err := readLine(rw, maxHandshakeBytes)
	if err != nil {
		return fmt.Errorf("fcvsock: read handshake reply: %w", err)
	}
	if !strings.HasPrefix(line, "OK ") {
		return fmt.Errorf("fcvsock: handshake rejected: %q", line)
	}
	return nil
}

// readLine reads bytes up to (and excluding) the first '\n', without
// over-reading past it, capped at max bytes. A line longer than max, or a
// read error before the newline, is an error.
func readLine(r io.Reader, max int) (string, error) {
	var b strings.Builder
	buf := make([]byte, 1)
	for {
		n, err := r.Read(buf)
		if n > 0 {
			if buf[0] == '\n' {
				return b.String(), nil
			}
			if b.Len() >= max {
				return "", fmt.Errorf("fcvsock: handshake reply exceeds %d bytes", max)
			}
			b.WriteByte(buf[0])
		}
		if err != nil {
			return "", err
		}
	}
}
