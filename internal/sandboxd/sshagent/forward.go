// Package sshagent forwards a HOST-held ssh-agent to a sandbox guest over the
// vsock control plane, exposing SIGNING ONLY. The private keys live on the
// host and never cross to the guest (SEC-01): the guest may enumerate public
// keys and request signatures, but every operation that would move or accept
// key material is refused. This is the host side of the FR-17.12 ssh-agent
// capability; the full guest round-trip over a real VM is exercised on the KVM
// box (MGIT-11.13). Refs: FR-17.12, SEC-01
package sshagent

import (
	"fmt"
	"io"

	"golang.org/x/crypto/ssh"
	"golang.org/x/crypto/ssh/agent"

	"github.com/hyper-swe/mgit/internal/model"
)

// Forwarder serves a signing-only view of a host ssh-agent to a guest over an
// injected transport (the vsock conn). It holds the host agent (which holds
// the keys) and wraps it so only List + Sign are honored. Refs: FR-17.12, SEC-01
type Forwarder struct {
	host agent.Agent
}

// NewForwarder wires the forwarder to a host-held agent. The host agent is the
// sole holder of key material; the forwarder never copies it. Refs: SEC-01
func NewForwarder(host agent.Agent) (*Forwarder, error) {
	if host == nil {
		return nil, fmt.Errorf("ssh-agent forward: host agent must not be nil")
	}
	return &Forwarder{host: host}, nil
}

// Serve speaks the ssh-agent protocol to the guest over conn, answering only
// signing-related requests against the host agent. It blocks until the guest
// closes the connection (or the transport errors). The conn is the vsock
// channel to one sandbox; key material never traverses it because the served
// agent is signOnly, which refuses every key-bearing operation. Refs: SEC-01
func (f *Forwarder) Serve(conn io.ReadWriter) error {
	if err := agent.ServeAgent(&signOnly{host: f.host}, conn); err != nil {
		return fmt.Errorf("ssh-agent forward: serve: %w", err)
	}
	return nil
}

// signOnly is the agent view exposed to the guest. It permits exactly two
// operations — List (public keys only) and Sign (a signature, never a key) —
// and refuses everything that could move key material onto the guest: Add
// (push a key in), Signers (hands out live signer objects that could leak
// material in-process), RemoveAll/Remove (mutate the host keyring), and
// Lock/Unlock (the unlock passphrase is host-side state). This is the SEC-01
// boundary: the host holds the keys; the guest gets signatures. Refs: SEC-01
type signOnly struct {
	host agent.Agent
}

// List returns the host's PUBLIC keys. Public key bytes are safe to share;
// they let the guest pick a key to sign with. Refs: SEC-01
func (s *signOnly) List() ([]*agent.Key, error) {
	return s.host.List()
}

// Sign asks the HOST agent to sign data with the host-held private key and
// returns only the resulting signature — the private key never leaves the
// host. Refs: SEC-01
func (s *signOnly) Sign(key ssh.PublicKey, data []byte) (*ssh.Signature, error) {
	return s.host.Sign(key, data)
}

// Add is refused: accepting a key from the guest would let a hostile guest
// seed or pivot the host agent. Refs: SEC-01
func (s *signOnly) Add(agent.AddedKey) error {
	return fmt.Errorf("%w: Add", model.ErrSSHKeyExtraction)
}

// Remove is refused: the guest may not mutate the host keyring. Refs: SEC-01
func (s *signOnly) Remove(ssh.PublicKey) error {
	return fmt.Errorf("%w: Remove", model.ErrSSHKeyExtraction)
}

// RemoveAll is refused: the guest may not mutate the host keyring. Refs: SEC-01
func (s *signOnly) RemoveAll() error {
	return fmt.Errorf("%w: RemoveAll", model.ErrSSHKeyExtraction)
}

// Lock is refused: agent lock state is host-side. Refs: SEC-01
func (s *signOnly) Lock([]byte) error {
	return fmt.Errorf("%w: Lock", model.ErrSSHKeyExtraction)
}

// Unlock is refused: agent lock state is host-side. Refs: SEC-01
func (s *signOnly) Unlock([]byte) error {
	return fmt.Errorf("%w: Unlock", model.ErrSSHKeyExtraction)
}

// Signers is refused: it would hand the guest live ssh.Signer objects bound to
// host key material. The guest signs only via Sign, which keeps the key on the
// host. Refs: SEC-01
func (s *signOnly) Signers() ([]ssh.Signer, error) {
	return nil, fmt.Errorf("%w: Signers", model.ErrSSHKeyExtraction)
}
