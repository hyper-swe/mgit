package libkrun

import (
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"os"
	"sync"
	"sync/atomic"

	"github.com/hyper-swe/mgit/internal/model"
)

const (
	// maxFrameBytes is the scratch buffer a frame is read into. One read
	// returns exactly one datagram regardless of size, so any frame-sized
	// buffer works; oversized frames are truncated, which is harmless for the
	// discard socket and cannot occur at the 1500-byte guest MTU.
	maxFrameBytes = 65536

	// denySocketRcvBufBytes gives the deny socket room to absorb a guest burst
	// before the kernel starts dropping datagrams. Dropping is fine (the
	// traffic is going nowhere); the buffer just keeps the guest's NIC from
	// seeing constant backpressure.
	denySocketRcvBufBytes = 1 << 20

	// denySocketMode keeps the backing socket owner-only, matching every other
	// unix socket mgit binds (FR-17.34).
	denySocketMode = 0o600

	// maxUnixSocketPath is the portable ceiling on a unix socket path: sun_path
	// is 104 bytes on darwin/BSD and 108 on Linux, minus the NUL. netBackingFor
	// checks it at resolve time so an over-long state dir fails with a reason
	// instead of a bare "invalid argument" out of bind(2).
	maxUnixSocketPath = 103
)

// hostNetPeer is the host end of a sandbox's virtio-net device, held for the
// VM's lifetime.
//
// Every mode needs one. libkrun accepts a net device whose backing socket has
// no peer (krun_add_net_unixgram returns 0 either way), but the VM then HANGS
// at boot — so an unserved socket is strictly worse than a refused launch.
// Refs: FR-17.7, SEC-04, ADR-010
type hostNetPeer interface{ Close() error }

// bindHostPeer binds the host end for a resolved backing, and is the only
// place that does so — a NIC is therefore never attached without a peer.
//
//   - "none" gets a socket that discards everything: the NIC has a willing
//     peer but no route anywhere.
//   - allowlist gets the userspace netstack gateway, which terminates the
//     guest's connections and admits only what the authorizer allows, and
//     serves the pinning DNS resolver at the gateway address. (Open mode is
//     refused earlier, by vmSpec.Validate: it has no authorizer assembly yet,
//     and a gateway with no policy is an open network.)
//
// Either way the peer is BOUND before the NIC is attached, because libkrun
// hangs at boot on a socket nothing is listening to.
// Refs: FR-17.7, FR-17.8, SEC-04, ADR-010
func bindHostPeer(backing netBacking, deps netDeps) (hostNetPeer, error) {
	if backing.Deny() {
		return bindDiscardSocket(backing.SocketPath, deps.logger)
	}
	gw, err := bindNetGateway(backing.SocketPath, deps)
	if err != nil {
		return nil, fmt.Errorf("%w: libkrun %s-mode host network: %w",
			model.ErrSandboxBackendUnavailable, backing.Mode, err)
	}
	return gw, nil
}

// discardSocket is the host end of a "none"-mode NIC: a bound unixgram socket
// that continuously drains and discards whatever the guest transmits.
//
// It is not an optimization — it is what makes "no network" bootable. With a
// bound, draining socket the VM boots normally and the guest's egress fails
// "network is unreachable"; with an unserved path it hangs. Draining matters
// as much as binding: an idle reader lets the datagram buffer fill, which
// wedges the guest the same way, only later. Measured on libkrun 1.19.4 —
// see ADR-010. Refs: FR-17.7, SEC-04, SEC-10, ADR-010
type discardSocket struct {
	conn   *net.UnixConn
	path   string
	logger *slog.Logger
	once   sync.Once
	err    error
	// closing marks a teardown in progress, so drain can tell the read error
	// Close provokes (normal) from one that leaves the NIC unserved (fatal
	// to the VM, and previously silent).
	closing atomic.Bool
}

// bindDiscardSocket binds the deny backing at path and starts draining it.
// A stale socket file from a crashed daemon is replaced rather than treated
// as a failure (per-sandbox state dirs mean the stale entry is always this
// sandbox's own). The caller owns the socket for the VM's lifetime and must
// Close it at teardown. Refs: FR-17.7, SEC-10
func bindDiscardSocket(path string, logger *slog.Logger) (*discardSocket, error) {
	if logger == nil {
		logger = slog.New(slog.NewTextHandler(io.Discard, nil))
	}
	conn, err := bindUnixgram(path, "deny socket")
	if err != nil {
		return nil, err
	}

	// Best-effort: the default buffer still works, it just drops sooner.
	_ = conn.SetReadBuffer(denySocketRcvBufBytes)

	d := &discardSocket{conn: conn, path: path, logger: logger}
	go d.drain()
	return d, nil
}

// drain reads and discards until the socket is closed, so the guest's NIC
// always has a willing peer.
//
// Exiting on the read error Close provokes is normal and stays quiet. Any
// OTHER read error also stops the drain, which leaves the guest's NIC
// unserved and wedges the VM — so that one is logged. It used to be silent,
// which made a hung sandbox indistinguishable from a slow one.
// Refs: MGIT-61.9 item 4, FR-17.7
func (d *discardSocket) drain() {
	buf := make([]byte, maxFrameBytes)
	for {
		if _, err := d.conn.Read(buf); err != nil {
			if !d.closing.Load() && !errors.Is(err, net.ErrClosed) {
				d.logger.Error("deny-socket drain stopped; the guest NIC is now unserved and the VM will hang",
					"event", "discard_drain_failed", "socket", d.path, "error", err.Error())
			}
			return
		}
	}
}

// Close stops draining and removes the socket file, leaving no residue
// (SEC-10). It is idempotent, and later calls report the first call's result.
func (d *discardSocket) Close() error {
	d.once.Do(func() {
		d.closing.Store(true)
		d.err = d.conn.Close()
		// Go unlinks a socket it bound, but a partially torn-down state must
		// not leave the path behind for the next launch to trip over.
		if err := os.Remove(d.path); err != nil && !os.IsNotExist(err) && d.err == nil {
			d.err = fmt.Errorf("remove deny socket %s: %w", d.path, err)
		}
	})
	return d.err
}

// bindUnixgram binds a host-side unixgram socket for a VM's NIC backing.
//
// Both host peers — the deny socket and the netstack gateway — need the same
// three steps in the same order, and got them by duplication: replace a stale
// socket file (a leftover from a crashed daemon must not make the next launch
// unbootable; the per-sandbox state dir means it is always this sandbox's
// own), bind, then restrict to owner-only like every other socket mgit binds
// (FR-17.34). kind names the socket in errors so a failure says WHICH one.
// Refs: FR-17.7, SEC-10
func bindUnixgram(path, kind string) (*net.UnixConn, error) {
	if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
		return nil, fmt.Errorf("clear stale %s %s: %w", kind, path, err)
	}
	conn, err := net.ListenUnixgram("unixgram", &net.UnixAddr{Name: path, Net: "unixgram"})
	if err != nil {
		return nil, fmt.Errorf("bind %s %s: %w", kind, path, err)
	}
	if err := os.Chmod(path, denySocketMode); err != nil {
		_ = conn.Close()
		return nil, fmt.Errorf("restrict %s %s: %w", kind, path, err)
	}
	return conn, nil
}
