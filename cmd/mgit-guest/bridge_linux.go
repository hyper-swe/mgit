//go:build linux

package main

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"net"
	"sync"
	"time"

	"github.com/mdlayher/vsock"
)

// bridgeDialTimeout bounds the guest-side dial to the local dev server so a
// host-initiated connection that finds no listener (the dev server not up
// yet) fails fast instead of pinning a goroutine.
const bridgeDialTimeout = 5 * time.Second

// portBridge forwards host-initiated AF_VSOCK connections on a guest vsock
// port to the guest's OWN loopback TCP service, completing one-way port
// publishing (SEC-09). It is deliberately asymmetric: it ONLY accepts
// host-dialed vsock connects and ONLY dials 127.0.0.1 inside the guest — it
// opens no path from the guest to the host. The guest holds no key (SEC-01)
// and the bridge moves only bytes between the host publisher and the guest's
// loopback, so it cannot be abused to reach a host service.
//
// Mechanism: the host publisher (portpub.Controller) does fcvsock.Dial(path,
// N) — a host->guest vsock connect to guest port N. The firecracker vsock
// device multiplexes by port with no extra host config, so the guest simply
// listens on AF_VSOCK port N and, per accept, dials TCP 127.0.0.1:N (the dev
// server) and copies bidirectionally until either side closes. One bridge per
// published port; all bridges die when the supervisor context is canceled
// (VM teardown), so there is no goroutine leak and no host residue.
// Refs: SEC-09, FR-17.8, SEC-01

// vsockListenFunc opens an AF_VSOCK listener on a guest port. A var seam so
// the accept loop is unit-testable with an in-memory listener (no real vsock
// device, CGO-free). The real implementation is mdlayher/vsock.Listen.
type vsockListenFunc func(port uint32) (net.Listener, error)

// tcpDialFunc dials the guest's own loopback TCP service for a published
// port. A var seam so tests can substitute an in-memory dialer; the real
// implementation dials 127.0.0.1:<port> and NOTHING else (SEC-09 one-way).
type tcpDialFunc func(ctx context.Context, port int) (net.Conn, error)

// realVsockListen listens on an AF_VSOCK port for host-initiated connections.
func realVsockListen(port uint32) (net.Listener, error) {
	return vsock.Listen(port, nil)
}

// realLoopbackDial dials the guest's OWN loopback (127.0.0.1) on the given
// port. It hard-codes the loopback host: the bridge must never dial anything
// but the guest's local dev server, so SEC-09 stays one-way regardless of the
// port. Refs: SEC-09
func realLoopbackDial(ctx context.Context, port int) (net.Conn, error) {
	var d net.Dialer
	return d.DialContext(ctx, "tcp", net.JoinHostPort("127.0.0.1", fmt.Sprintf("%d", port)))
}

// servePublishBridges starts one AF_VSOCK->TCP-loopback bridge per published
// guest port and blocks until ctx is canceled, then waits for every bridge to
// drain. A port that fails to listen is logged and skipped (best-effort: a
// single un-bindable port must not wedge the others or boot). All bridges are
// tied to ctx, so they end with the VM. Refs: SEC-09, FR-17.8
func servePublishBridges(ctx context.Context, ports []int, listen vsockListenFunc, dial tcpDialFunc, logger *slog.Logger) {
	if len(ports) == 0 {
		return
	}
	var wg sync.WaitGroup
	for _, port := range ports {
		ln, err := listen(uint32(port)) //nolint:gosec // OK: port is range-checked (1..65535) by guestboot.ParsePublishPorts
		if err != nil {
			logger.Error("mgit-guest publish bridge listen failed",
				"event", "publish_bridge_error", "guest_port", port, "error", err.Error())
			continue
		}
		wg.Add(1)
		go func(port int, ln net.Listener) {
			defer wg.Done()
			runPublishBridge(ctx, port, ln, dial, logger)
		}(port, ln)
	}
	logger.Info("mgit-guest publish bridges started", "event", "publish_bridges_started", "count", len(ports))
	wg.Wait()
}

// runPublishBridge accepts host-initiated vsock connections on one port and
// forwards each to the guest's loopback dev server until ctx is canceled.
// Closing the listener on cancel unblocks Accept. Refs: SEC-09
func runPublishBridge(ctx context.Context, port int, ln net.Listener, dial tcpDialFunc, logger *slog.Logger) {
	defer func() { _ = ln.Close() }()
	go func() {
		<-ctx.Done()
		_ = ln.Close()
	}()
	for {
		conn, err := ln.Accept()
		if err != nil {
			select {
			case <-ctx.Done():
				return
			default:
				logger.Error("mgit-guest publish bridge accept failed",
					"event", "publish_bridge_error", "guest_port", port, "error", err.Error())
				return
			}
		}
		go forwardToLoopback(ctx, port, conn, dial, logger)
	}
}

// forwardToLoopback dials the guest's loopback dev server for one accepted
// vsock connection and copies bytes both ways until either side closes. The
// vsock side (host-initiated) is the only inbound; the TCP side is always the
// guest's own 127.0.0.1 — there is no outbound path to the host. Refs: SEC-09
func forwardToLoopback(ctx context.Context, port int, vconn net.Conn, dial tcpDialFunc, logger *slog.Logger) {
	defer func() { _ = vconn.Close() }()
	dctx, cancel := context.WithTimeout(ctx, bridgeDialTimeout)
	defer cancel()
	tconn, err := dial(dctx, port)
	if err != nil {
		// No loopback dev server listening (e.g. not up yet): drop the host
		// connection. Bounded, no retry — the host publisher retries.
		logger.Debug("mgit-guest publish bridge dial failed",
			"event", "publish_bridge_dial", "guest_port", port, "error", err.Error())
		return
	}
	defer func() { _ = tconn.Close() }()
	copyDuplex(vconn, tconn)
}

// copyDuplex copies bytes in both directions between two connections until
// either side closes, then unblocks the other copy by closing both ends. It
// returns once both directions have finished. Refs: SEC-09
func copyDuplex(a, b net.Conn) {
	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		_, _ = io.Copy(a, b)
		_ = a.Close()
		_ = b.Close()
	}()
	go func() {
		defer wg.Done()
		_, _ = io.Copy(b, a)
		_ = a.Close()
		_ = b.Close()
	}()
	wg.Wait()
}
