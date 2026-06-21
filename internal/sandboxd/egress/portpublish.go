package egress

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
)

// GuestPortDialer opens a connection to a port the guest published (its dev
// server). In production this dials the guest's forwarder over vsock;
// injected so the publisher is testable without a live guest. Refs: SEC-09
type GuestPortDialer interface {
	DialGuestPort(ctx context.Context, port int) (net.Conn, error)
}

// PublisherConfig wires the one-way port publisher.
type PublisherConfig struct {
	Dialer GuestPortDialer
	Logger *slog.Logger
}

// Publisher forwards guest-published ports to HOST LOOPBACK, one way only:
// it opens a 127.0.0.1 listener per published port and, on each host
// accept, dials INTO the guest and splices. There is deliberately no
// reverse path — the guest can never use this to open a connection to a
// host loopback service (e.g. a host DB on 127.0.0.1:5432); that direction
// has no listener and the guest's only egress (the proxy) denies loopback.
// Refs: SEC-09
type Publisher struct {
	cfg PublisherConfig
}

// NewPublisher validates the configuration and returns a Publisher.
func NewPublisher(cfg PublisherConfig) (*Publisher, error) {
	switch {
	case cfg.Dialer == nil:
		return nil, fmt.Errorf("port publisher: guest dialer must not be nil")
	case cfg.Logger == nil:
		return nil, fmt.Errorf("port publisher: logger must not be nil")
	}
	return &Publisher{cfg: cfg}, nil
}

// Publish opens a host loopback listener for a guest-published port and
// serves it until the returned listener is closed. The caller closes the
// listener to stop forwarding. Binding is 127.0.0.1 only, so the published
// service is reachable at host localhost:<port> but never on an external
// interface. Refs: SEC-09, FR-17.7
func (p *Publisher) Publish(ctx context.Context, port int) (net.Listener, error) {
	if port < 1 || port > 65535 {
		return nil, fmt.Errorf("port publisher: port %d out of range (1..65535)", port)
	}
	var lc net.ListenConfig
	ln, err := lc.Listen(ctx, "tcp", fmt.Sprintf("127.0.0.1:%d", port))
	if err != nil {
		return nil, fmt.Errorf("port publisher: listen 127.0.0.1:%d: %w", port, err)
	}
	go p.serve(ctx, ln, port)
	return ln, nil
}

// serve accepts host connections and forwards each into the guest. Every
// connection is host-initiated (one-way). Refs: SEC-09
func (p *Publisher) serve(ctx context.Context, ln net.Listener, port int) {
	for {
		conn, err := ln.Accept()
		if err != nil {
			if errors.Is(err, net.ErrClosed) || ctx.Err() != nil {
				return
			}
			p.cfg.Logger.Warn("port publish: accept failed", "event", "publish_accept", "port", port, "error", err.Error())
			return
		}
		go p.forward(ctx, conn, port)
	}
}

// forward dials the guest's published port and splices the host connection
// to it. The dial is host-initiated; on dial failure the host connection is
// closed (no half-open leak). Refs: SEC-09
func (p *Publisher) forward(ctx context.Context, host net.Conn, port int) {
	guest, err := p.cfg.Dialer.DialGuestPort(ctx, port)
	if err != nil {
		p.cfg.Logger.Warn("port publish: guest dial failed", "event", "publish_dialfail", "port", port, "error", err.Error())
		_ = host.Close()
		return
	}
	splice(host, guest)
}
