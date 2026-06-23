package sshagent

import (
	"bytes"
	"crypto/ed25519"
	"crypto/rand"
	"errors"
	"io"
	"net"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"golang.org/x/crypto/ssh"
	"golang.org/x/crypto/ssh/agent"

	"github.com/hyper-swe/mgit/internal/model"
)

// newHostAgent returns a host-side in-memory ssh-agent holding one ed25519
// key, plus the raw private key bytes so a test can assert they never cross
// the wire.
func newHostAgent(t *testing.T) (agent.Agent, ed25519.PrivateKey, ssh.PublicKey) {
	t.Helper()
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	require.NoError(t, err)
	keyring := agent.NewKeyring()
	require.NoError(t, keyring.Add(agent.AddedKey{PrivateKey: priv}))
	signer, err := ssh.NewSignerFromKey(priv)
	require.NoError(t, err)
	_ = pub
	return keyring, priv, signer.PublicKey()
}

// tapConn wraps a net.Conn and records every byte that flows in each
// direction so a test can scan the wire for leaked key material.
type tapConn struct {
	net.Conn
	read  *bytes.Buffer // bytes the guest sent to the host
	write *bytes.Buffer // bytes the host sent to the guest
}

func (c *tapConn) Read(b []byte) (int, error) {
	n, err := c.Conn.Read(b)
	c.read.Write(b[:n])
	return n, err
}

func (c *tapConn) Write(b []byte) (int, error) {
	c.write.Write(b)
	return c.Conn.Write(b)
}

// TestCap_SSHAgentForward_KeysStayHost proves SEC-01: the host-side ssh-agent
// forward exposes SIGNING ONLY. The guest can list public keys and request
// signatures, but (a) raw private-key material never crosses the vsock, and
// (b) any guest attempt to ADD/extract a key is refused — the keys stay on
// the host.
func TestCap_SSHAgentForward_KeysStayHost(t *testing.T) {
	t.Parallel()

	hostAgent, hostPriv, hostPub := newHostAgent(t)

	// vsock stand-in: a bidirectional pipe. host end is tapped so we can
	// scan the full byte stream that crosses to the guest.
	guestEnd, hostEnd := net.Pipe()
	defer func() { _ = guestEnd.Close() }() // test cleanup
	tap := &tapConn{Conn: hostEnd, read: &bytes.Buffer{}, write: &bytes.Buffer{}}

	fwd, err := NewForwarder(hostAgent)
	require.NoError(t, err)

	serveErr := make(chan error, 1)
	go func() { serveErr <- fwd.Serve(tap) }()

	// Guest side speaks the ssh-agent protocol over the vsock.
	guest := agent.NewClient(guestEnd)

	// (1) The guest may LIST public keys (no private material).
	keys, err := guest.List()
	require.NoError(t, err)
	require.Len(t, keys, 1)
	assert.Equal(t, hostPub.Marshal(), keys[0].Marshal())

	// (2) The guest may request a SIGNATURE; the host signs with the held
	// key and only the signature crosses back.
	payload := []byte("commit object to sign")
	sig, err := guest.Sign(keys[0], payload)
	require.NoError(t, err)
	require.NoError(t, hostPub.Verify(payload, sig), "signature must verify against the host public key")

	// (3) SEC-01 core: a guest attempt to ADD a key (the lever to push
	// material across / take over the agent) is refused.
	_, otherPriv, err := ed25519.GenerateKey(rand.Reader)
	require.NoError(t, err)
	addErr := guest.Add(agent.AddedKey{PrivateKey: otherPriv})
	require.Error(t, addErr, "the forward must refuse key Add (signing-only)")

	_ = guestEnd.Close() // unblock Serve; close error is irrelevant to the test
	select {
	case err := <-serveErr:
		if err != nil && !errors.Is(err, io.EOF) && !errors.Is(err, io.ErrClosedPipe) {
			// ServeAgent returns the transport error on close; tolerate it.
			assert.True(t, isBenignClose(err), "unexpected serve error: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("forwarder did not stop after the guest closed the connection")
	}

	// (4) The decisive assertion: the raw private key NEVER appeared on the
	// wire in either direction.
	rawSeed := hostPriv.Seed()
	assert.NotContains(t, tap.write.Bytes(), rawSeed, "private seed must never be sent to the guest")
	assert.NotContains(t, tap.read.Bytes(), rawSeed, "private seed must never appear on the wire")
	assert.NotContains(t, tap.write.Bytes(), []byte(hostPriv), "private key bytes must never be sent to the guest")
}

// TestNewForwarder_NilAgent_Error proves the forwarder requires a host agent.
func TestNewForwarder_NilAgent_Error(t *testing.T) {
	t.Parallel()
	_, err := NewForwarder(nil)
	require.Error(t, err)
}

// TestSignOnly_RefusesKeyBearingOps proves the SEC-01 boundary directly: every
// operation that could move or mutate host key material is refused with
// ErrSSHKeyExtraction, while only List and Sign are honored against the host
// agent. This is the host-side guarantee that signing is the ONLY capability
// the guest gets.
func TestSignOnly_RefusesKeyBearingOps(t *testing.T) {
	t.Parallel()

	hostAgent, _, hostPub := newHostAgent(t)
	so := &signOnly{host: hostAgent}

	// Honored: List (public keys) and Sign (a signature).
	keys, err := so.List()
	require.NoError(t, err)
	require.Len(t, keys, 1)
	sig, err := so.Sign(keys[0], []byte("data"))
	require.NoError(t, err)
	require.NoError(t, hostPub.Verify([]byte("data"), sig))

	// Refused: every key-bearing / mutating operation.
	_, otherPriv, err := ed25519.GenerateKey(rand.Reader)
	require.NoError(t, err)
	assert.ErrorIs(t, so.Add(agent.AddedKey{PrivateKey: otherPriv}), model.ErrSSHKeyExtraction)
	assert.ErrorIs(t, so.Remove(hostPub), model.ErrSSHKeyExtraction)
	assert.ErrorIs(t, so.RemoveAll(), model.ErrSSHKeyExtraction)
	assert.ErrorIs(t, so.Lock([]byte("pw")), model.ErrSSHKeyExtraction)
	assert.ErrorIs(t, so.Unlock([]byte("pw")), model.ErrSSHKeyExtraction)
	signers, err := so.Signers()
	assert.Nil(t, signers, "Signers must not hand out live signer objects")
	assert.ErrorIs(t, err, model.ErrSSHKeyExtraction)
}

func isBenignClose(err error) bool {
	return errors.Is(err, io.EOF) || errors.Is(err, io.ErrClosedPipe) || errors.Is(err, net.ErrClosed)
}
