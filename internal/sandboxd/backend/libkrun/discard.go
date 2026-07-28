package libkrun

import (
	"fmt"
	"net"
	"os"
	"sync"

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
//   - allowlist/open get the userspace netstack gateway, which terminates the
//     guest's connections and admits only what the authorizer allows. TCP
//     ONLY today: there is no UDP forwarder, so guest DNS does not resolve
//     and allowlisted NAMES are unreachable — only IP/CIDR entries work.
//
// Either way the peer is BOUND before the NIC is attached, because libkrun
// hangs at boot on a socket nothing is listening to.
// Refs: FR-17.7, FR-17.8, SEC-04, ADR-010
func bindHostPeer(backing netBacking, auth flowAuthorizer, dns dnsResolver) (hostNetPeer, error) {
	if backing.Deny() {
		return bindDiscardSocket(backing.SocketPath)
	}
	gw, err := bindNetGateway(backing.SocketPath, auth, dns)
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
	conn *net.UnixConn
	path string
	once sync.Once
	err  error
}

// bindDiscardSocket binds the deny backing at path and starts draining it.
// A stale socket file from a crashed daemon is replaced rather than treated
// as a failure (per-sandbox state dirs mean the stale entry is always this
// sandbox's own). The caller owns the socket for the VM's lifetime and must
// Close it at teardown. Refs: FR-17.7, SEC-10
func bindDiscardSocket(path string) (*discardSocket, error) {
	// A unix socket cannot be bound over an existing path; a leftover from a
	// previous run must not make the next launch unbootable.
	if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
		return nil, fmt.Errorf("clear stale deny socket %s: %w", path, err)
	}

	conn, err := net.ListenUnixgram("unixgram", &net.UnixAddr{Name: path, Net: "unixgram"})
	if err != nil {
		return nil, fmt.Errorf("bind deny socket %s: %w", path, err)
	}

	if err := os.Chmod(path, denySocketMode); err != nil {
		_ = conn.Close()
		return nil, fmt.Errorf("restrict deny socket %s: %w", path, err)
	}

	// Best-effort: the default buffer still works, it just drops sooner.
	_ = conn.SetReadBuffer(denySocketRcvBufBytes)

	d := &discardSocket{conn: conn, path: path}
	go d.drain()
	return d, nil
}

// drain reads and discards until the socket is closed, so the guest's NIC
// always has a willing peer. It exits on the read error Close provokes; any
// other read error also stops the drain, which would leave the guest's NIC
// unserved — surfacing that needs the injected logger the backend gets when
// the Hypervisor is wired (MGIT-61.6).
func (d *discardSocket) drain() {
	buf := make([]byte, maxFrameBytes)
	for {
		if _, err := d.conn.Read(buf); err != nil {
			return
		}
	}
}

// Close stops draining and removes the socket file, leaving no residue
// (SEC-10). It is idempotent, and later calls report the first call's result.
func (d *discardSocket) Close() error {
	d.once.Do(func() {
		d.err = d.conn.Close()
		// Go unlinks a socket it bound, but a partially torn-down state must
		// not leave the path behind for the next launch to trip over.
		if err := os.Remove(d.path); err != nil && !os.IsNotExist(err) && d.err == nil {
			d.err = fmt.Errorf("remove deny socket %s: %w", d.path, err)
		}
	})
	return d.err
}
