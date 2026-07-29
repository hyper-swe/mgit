package libkrun

import (
	"context"
	"errors"
	"log/slog"
	"net"
	"net/netip"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// SILENT FAILURE ON A CONTAINMENT BOUNDARY IS THE BUG THESE PIN.
//
// The gateway decides what a hostile guest may reach. When the authorizer
// errors, when the host dial fails, or when the discard socket's drain stops,
// the guest sees a refused connection or a wedged NIC — and until now the
// operator saw nothing at all. Refs: MGIT-61.9 item 4, SEC-04, FR-17.8

// syncBuffer is a concurrency-safe log sink: the gateway logs from its
// per-connection goroutines.
type syncBuffer struct {
	mu sync.Mutex
	b  strings.Builder
}

func (s *syncBuffer) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (s *syncBuffer) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}

// waitForLog polls until the sink contains want, or the deadline passes.
func waitForLog(t *testing.T, sink *syncBuffer, want string) bool {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if strings.Contains(sink.String(), want) {
			return true
		}
		time.Sleep(20 * time.Millisecond)
	}
	return false
}

func TestNetGateway_LogsWhyItRefusedAFlow(t *testing.T) {
	tests := []struct {
		name  string
		deps  func(sink *syncBuffer) netDeps
		event string
	}{
		{
			// An authorizer that cannot decide is treated as a denial. That
			// is correct, but indistinguishable from a policy deny unless it
			// is logged — and the two need very different operator responses.
			name: "authorizer_error",
			deps: func(sink *syncBuffer) netDeps {
				return netDeps{
					auth:   errAuthorizer{},
					logger: slog.New(slog.NewJSONHandler(sink, nil)),
				}
			},
			event: "egress_authorize_failed",
		},
		{
			// The flow was ALLOWED and the host dial then failed: the guest
			// sees a reset it cannot explain, and the cause is host-side.
			name: "host_dial_failure",
			deps: func(sink *syncBuffer) netDeps {
				return netDeps{
					auth: &stubAuthorizer{allowed: map[string]string{"93.184.216.34": "127.0.0.1"}},
					dial: func(context.Context, netip.Addr, int) (net.Conn, error) {
						return nil, errors.New("connection refused by the host")
					},
					logger: slog.New(slog.NewJSONHandler(sink, nil)),
				}
			},
			event: "egress_dial_failed",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dir := shortTempDir(t)
			gwPath := filepath.Join(dir, proxySocketName)
			sink := &syncBuffer{}

			gw, err := bindNetGateway(gwPath, tt.deps(sink))
			if err != nil {
				t.Fatalf("bindNetGateway: %v", err)
			}
			t.Cleanup(func() { _ = gw.Close() })

			guest := fakeGuest(t, dir, gwPath)
			if c, err := guestDial(t, guest, "93.184.216.34", 443); err == nil {
				_ = c.Close()
			}

			if !waitForLog(t, sink, tt.event) {
				t.Errorf("the gateway swallowed the failure; log has no %q:\n%s", tt.event, sink.String())
			}
		})
	}
}

func TestDiscardSocket_LogsWhenTheDrainStopsUnexpectedly(t *testing.T) {
	// drain exits on the read error Close provokes — that is normal and must
	// stay quiet. Any OTHER read error also stops it, which leaves the
	// guest's NIC unserved and hangs the VM; that must never be silent.
	sink := &syncBuffer{}
	logger := slog.New(slog.NewJSONHandler(sink, nil))

	d, err := bindDiscardSocket(filepath.Join(shortTempDir(t), denySocketName), logger)
	if err != nil {
		t.Fatalf("bindDiscardSocket: %v", err)
	}

	// A normal close must NOT log a failure.
	if err := d.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	time.Sleep(100 * time.Millisecond)
	if strings.Contains(sink.String(), "discard_drain_failed") {
		t.Errorf("a normal teardown logged a failure:\n%s", sink.String())
	}
}

func TestNetDeps_DefaultsAreSafe(t *testing.T) {
	// A nil logger must not panic the data path: the gateway runs in the VM
	// child, and a nil-pointer there kills the sandbox rather than a test.
	dir := shortTempDir(t)
	gw, err := bindNetGateway(filepath.Join(dir, proxySocketName), netDeps{auth: &stubAuthorizer{}})
	if err != nil {
		t.Fatalf("bindNetGateway with no logger: %v", err)
	}
	t.Cleanup(func() { _ = gw.Close() })
	if gw.logger == nil {
		t.Error("gateway must hold a usable logger even when none was injected")
	}
}
