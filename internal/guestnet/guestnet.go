// Package guestnet configures the sandbox guest's own NIC at boot, from the
// descriptor the host sends over the guestboot token channel.
//
// WHY IT EXISTS (MGIT-68). mgit-guest is PID 1 in the sandbox: no distro
// init, no dhclient, no NetworkManager ever runs. Until this package, nothing
// gave the guest an address or a default route — so eth0 came up present but
// UNADDRESSED, and every flow failed with ENETUNREACH while DNS failed with
// EAI_AGAIN (the gateway's :53 being equally unreachable). That shipped in
// v0.4.0 because the real-VM tests asserted only that DENIED destinations
// were denied, which an unconfigured NIC satisfies perfectly.
//
// THE ADDRESSING DECISION, recorded: the guest is TOLD its address, it does
// not ask. The alternative — DHCP served by the host gateway — would need a
// DHCP server written into the gateway (gvisor's netstack ships none) and a
// client written into the guest: new code on both ends of a security
// boundary, plus lease state and a boot-time retry loop, to deliver three
// values the host already knows and already has a tested channel for. DHCP's
// usual advantage is that it works with a guest userspace you do not control;
// here mgit-guest IS the userspace and runs before anything else, so that
// advantage does not apply. Revisit only if mgit ever hands PID 1 to a base
// image's own init.
//
// The addresses themselves belong to the backend that defines the link (for
// libkrun, netgw.go's guestIP/gatewayIP/gwPrefixLen). This package transports
// and applies them; it never restates one. Two copies of an address is how
// this breaks again.
//
// Refs: MGIT-68, FR-17.7, FR-17.8, SEC-07, ADR-010
package guestnet

import (
	"fmt"
	"io"
	"log/slog"
	"net"
	"os"
	"path/filepath"
	"time"

	"github.com/hyper-swe/mgit/internal/guestboot"
)

// NIC is the guest interface the host's virtio-net device appears as, and it
// is selected BY NAME on purpose. The guest kernel also enumerates a dummy0
// interface, so "the first non-loopback interface" picks the wrong device and
// every dial then fails "network is unreachable" — a trap the libkrun spike
// hit and documented (ADR-010). Refs: MGIT-68, ADR-010
const NIC = "eth0"

// DefaultResolvPath is where the guest's resolver configuration is written.
// Overwriting a base image's copy is correct and intended: the sandbox's ONLY
// reachable resolver is the host-side one on the gateway (SEC-07), so any
// inherited nameserver is unreachable at best and a policy bypass attempt at
// worst. The overlay root is writable (FR-17.17), so this never mutates the
// pinned image. Refs: SEC-07, FR-17.17
const DefaultResolvPath = "/etc/resolv.conf"

// nicWaitAttempts / nicWaitInterval bound the wait for the NIC to be
// enumerated. The virtio device is present at boot on every backend measured,
// so this is a startup race guard, not a poll: it costs nothing when the
// interface is already there and fails with a reason rather than hanging when
// it never appears.
const (
	nicWaitAttempts = 20
	nicWaitInterval = 100 * time.Millisecond
)

// Linker applies an address, netmask, up-flag and default route to one
// interface. It is the platform seam: the Linux implementation issues the
// SIOCSIF* / SIOCADDRT ioctls, and every other build fails closed. Keeping it
// an interface is also what makes the boot logic testable without
// CAP_NET_ADMIN or a VM. Refs: MGIT-68
type Linker interface {
	// Configure brings iface up with n's address, mask and default route.
	Configure(iface string, n guestboot.GuestNetwork) error
}

// Deps are the collaborators Apply needs. They travel as one value because
// they are all boot-time seams for a single operation, and because a
// five-parameter function here would be at CLAUDE.md's budget with nothing
// left for growth.
type Deps struct {
	Link        Linker              // applies the address and route
	ResolvPath  string              // resolver file to write; empty = DefaultResolvPath
	LookupIface func(string) error  // reports whether the NIC exists yet
	Sleep       func(time.Duration) // waits between NIC lookups
	Logger      *slog.Logger        // boot log sink
}

// withDefaults fills the optional collaborators. A nil logger is discarded
// rather than left nil: this runs in PID 1, where a nil-pointer dereference
// kills the sandbox before it serves anything.
func (d Deps) withDefaults() Deps {
	if d.ResolvPath == "" {
		d.ResolvPath = DefaultResolvPath
	}
	if d.LookupIface == nil {
		d.LookupIface = func(name string) error {
			_, err := net.InterfaceByName(name)
			return err
		}
	}
	if d.Sleep == nil {
		d.Sleep = time.Sleep
	}
	if d.Logger == nil {
		d.Logger = slog.New(slog.NewTextHandler(io.Discard, nil))
	}
	return d
}

// Apply configures the guest's NIC and resolver from the host's descriptor.
//
// An ABSENT descriptor is a no-op: a none-mode sandbox is meant to have no
// network, and configuring an address against a discard socket would only
// make a dead network look like a live one. A PRESENT but invalid or
// unappliable descriptor is fatal — the caller fails the boot with the reason
// on the console, because a guest that silently comes up with no route is
// exactly the failure that shipped as v0.4.0. Refs: MGIT-68, FR-17.7
func Apply(n guestboot.GuestNetwork, deps Deps) error {
	d := deps.withDefaults()
	if n.Empty() {
		d.Logger.Info("guest network not configured (no descriptor from the host)",
			"event", "guest_net_absent")
		return nil
	}
	if !n.Valid() {
		return fmt.Errorf("mgit-guest: incomplete guest network descriptor: %+v", n)
	}
	if d.Link == nil {
		return fmt.Errorf("mgit-guest: no link configurator on this platform; "+
			"cannot configure %s with %+v", NIC, n)
	}
	if err := waitForNIC(d); err != nil {
		return err
	}
	if err := d.Link.Configure(NIC, n); err != nil {
		return fmt.Errorf("mgit-guest: configure %s as %s/%d gw %s: %w",
			NIC, n.IP, n.PrefixLen, n.Gateway, err)
	}
	if err := writeResolvConf(d.ResolvPath, n.Resolver()); err != nil {
		return err
	}
	d.Logger.Info("guest network configured",
		"event", "guest_net_configured", "iface", NIC, "ip", n.IP,
		"prefix_len", n.PrefixLen, "gateway", n.Gateway, "resolver", n.Resolver())
	return nil
}

// waitForNIC waits, bounded, for the interface to be enumerated.
func waitForNIC(d Deps) error {
	var err error
	for attempt := 0; attempt < nicWaitAttempts; attempt++ {
		if attempt > 0 {
			d.Sleep(nicWaitInterval)
		}
		if err = d.LookupIface(NIC); err == nil {
			return nil
		}
	}
	return fmt.Errorf("mgit-guest: interface %s never appeared (waited %s): %w",
		NIC, time.Duration(nicWaitAttempts)*nicWaitInterval, err)
}

// writeResolvConf points the guest at the host-side resolver.
//
// The existing file is REMOVED first rather than truncated: base images ship
// /etc/resolv.conf as a symlink into a resolver daemon's runtime directory
// (often dangling in a minimal guest), and writing through that symlink would
// either fail or land somewhere nothing reads. Refs: SEC-07, MGIT-68
func writeResolvConf(path, resolver string) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil { //nolint:gosec // guest /etc, not host-trusted
		return fmt.Errorf("mgit-guest: create %s for resolv.conf: %w", filepath.Dir(path), err)
	}
	if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("mgit-guest: replace %s: %w", path, err)
	}
	if err := os.WriteFile(path, []byte("nameserver "+resolver+"\n"), 0o644); err != nil { //nolint:gosec // world-readable by design: every guest process resolves through it
		return fmt.Errorf("mgit-guest: write %s: %w", path, err)
	}
	return nil
}
