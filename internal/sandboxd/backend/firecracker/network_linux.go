//go:build linux

package firecracker

import (
	"context"
	"fmt"
	"os"
	"os/exec"
)

// procNetRoute is the kernel's IPv4 routing table. Reading it (rather than
// execing `ip route`) keeps default-route detection dependency-free and the
// parsing logic unit-testable. Refs: FR-17.7
const procNetRoute = "/proc/net/route"

// defaultRouteIface returns the host's preferred default-route interface, the
// one open mode NATs the guest out through. An absent default route is an
// error so open mode fails closed rather than NATing through nothing.
// Refs: FR-17.7
func defaultRouteIface() (string, error) {
	f, err := os.Open(procNetRoute)
	if err != nil {
		return "", fmt.Errorf("open %s: %w", procNetRoute, err)
	}
	defer func() { _ = f.Close() }()
	return parseDefaultRouteIface(f)
}

// ipRunner is the real NetRunner: it execs the privileged host network
// tools (ip, iptables, sysctl) to realize a tap plan. Argv is built entirely
// from manager-owned values (sandbox ID-derived names/IPs, fixed ports) —
// never from guest input — and runs without a shell. Requires root (the
// daemon runs privileged for KVM). Refs: FR-17.7, SEC-04
type ipRunner struct{}

func (ipRunner) Run(ctx context.Context, name string, args ...string) error {
	out, err := exec.CommandContext(ctx, name, args...).CombinedOutput() //nolint:gosec // argv is manager-owned (no shell, no guest input)
	if err != nil {
		return fmt.Errorf("%s %v: %w: %s", name, args, err, out)
	}
	return nil
}
