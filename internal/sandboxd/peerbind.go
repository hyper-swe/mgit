package sandboxd

import (
	"fmt"
	"log/slog"
	"sync"

	"github.com/hyper-swe/mgit/internal/model"
)

// PeerBinder binds each sandbox's control/land/attestation channel to its
// hypervisor peer identity — the vsock CID on AF_VSOCK backends (KVM,
// Virtualization.framework) or the VM-GUID on Hyper-V sockets — and
// authorizes incoming connections against that binding. The peer identity
// is an opaque string the transport supplies (e.g. "cid:3" or a GUID), so
// one binder serves every backend. mgit-sandboxd rejects any connection
// whose source peer differs from the addressed sandbox's binding, so one
// guest can never reach another's land or attestation channel (SEC-10).
// Bindings are invalidated at teardown so a recycled CID/GUID assigned to
// a successor VM cannot inherit a destroyed sandbox's channel (FR-17.27).
// Refs: FR-17.27, SEC-10, MGIT-11.8.6
type PeerBinder struct {
	mu       sync.Mutex
	bindings map[string]string // sandbox ID -> bound peer identity
	logger   *slog.Logger
}

// NewPeerBinder returns an empty binder.
func NewPeerBinder(logger *slog.Logger) *PeerBinder {
	return &PeerBinder{bindings: make(map[string]string), logger: logger}
}

// Bind records (or replaces, on relaunch) a sandbox's peer identity at
// launch. Refs: FR-17.27
func (b *PeerBinder) Bind(sandboxID, peerID string) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.bindings[sandboxID] = peerID
}

// Invalidate drops a sandbox's binding at teardown, so a connection that
// still addresses it — or a recycled CID/GUID handed to a successor —
// cannot reach the destroyed channel. Refs: FR-17.27
func (b *PeerBinder) Invalidate(sandboxID string) {
	b.mu.Lock()
	defer b.mu.Unlock()
	delete(b.bindings, sandboxID)
}

// Authorize verifies that a connection's source peer identity matches the
// binding of the sandbox it addresses. It fails closed: an unbound
// sandbox (never launched or torn down) and an empty/unverifiable source
// peer are both rejected. Every rejection is audited; a mismatch returns
// ErrPeerBindingMismatch. Refs: FR-17.27, SEC-10
func (b *PeerBinder) Authorize(addressedSandboxID, sourcePeerID string) error {
	b.mu.Lock()
	bound, ok := b.bindings[addressedSandboxID]
	b.mu.Unlock()

	switch {
	case !ok:
		return b.reject(addressedSandboxID, sourcePeerID, "no binding for addressed sandbox")
	case sourcePeerID == "":
		return b.reject(addressedSandboxID, sourcePeerID, "unverifiable (empty) source peer")
	case sourcePeerID != bound:
		return b.reject(addressedSandboxID, sourcePeerID, "source peer does not match binding")
	}
	return nil
}

// reject audits and returns the binding-mismatch error. The source peer
// identity is host-observed (the transport's reported CID/GUID), never
// guest-asserted text, so logging it is safe (SEC-05).
func (b *PeerBinder) reject(sandboxID, sourcePeerID, reason string) error {
	b.logger.Error("sandboxd rejected channel peer",
		"event", "peer_rejected", "sandbox_id", sandboxID,
		"source_peer", sourcePeerID, "reason", reason)
	return fmt.Errorf("%w: sandbox %s: %s", model.ErrPeerBindingMismatch, sandboxID, reason)
}
