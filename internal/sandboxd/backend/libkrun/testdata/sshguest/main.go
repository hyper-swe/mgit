// Command sshguest is the GUEST side of litmus leg 1: it runs a real SSH
// server inside a real libkrun microVM, reachable ONLY through the SEC-09
// one-way published port.
//
// It listens on AF_VSOCK rather than TCP because that is the published
// transport: libkrun listens on a host unix socket for the port and forwards
// inbound connections to this guest vsock port. The direction is the control
// — the host initiates, the guest only accepts, and nothing here opens a path
// out. Refs: SEC-09, FR-17.8, MGIT-61.10
package main

import (
	"crypto/ed25519"
	"crypto/rand"
	"fmt"
	"net"
	"os"
	"strconv"

	"github.com/mdlayher/vsock"
	"golang.org/x/crypto/ssh"
)

// publishedPort is the guest vsock port the host publishes. It matches the
// port the test asks the backend to publish.
const publishedPort = 8022

// banner is what a successful `exec` returns, so the host can prove it drove
// a real SSH session rather than merely completing a TCP connect.
const banner = "hello-from-guest-sshd"

// hostKey generates a throwaway host key for this one boot.
func hostKey() (ssh.Signer, error) {
	_, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return nil, err
	}
	return ssh.NewSignerFromKey(priv)
}

// serve answers one SSH connection: accept the session channel, reply to
// "exec" with the banner, and exit 0.
func serve(nConn net.Conn, cfg *ssh.ServerConfig) {
	defer func() { _ = nConn.Close() }()
	conn, chans, reqs, err := ssh.NewServerConn(nConn, cfg)
	if err != nil {
		fmt.Printf("GUEST: ssh handshake failed: %v\n", err)
		return
	}
	defer func() { _ = conn.Close() }()
	go ssh.DiscardRequests(reqs)

	for newCh := range chans {
		if newCh.ChannelType() != "session" {
			_ = newCh.Reject(ssh.UnknownChannelType, "only sessions")
			continue
		}
		ch, chReqs, err := newCh.Accept()
		if err != nil {
			return
		}
		go func() {
			for req := range chReqs {
				if req.Type == "exec" {
					_, _ = ch.Write([]byte(banner))
					_ = req.Reply(true, nil)
					_, _ = ch.SendRequest("exit-status", false, ssh.Marshal(struct{ Status uint32 }{0}))
					_ = ch.Close()
					continue
				}
				_ = req.Reply(false, nil)
			}
		}()
	}
}

func main() {
	fmt.Println("GUEST: booted inside a real libkrun microVM")

	signer, err := hostKey()
	if err != nil {
		fmt.Printf("GUEST-RESULT SSHD = FAILED (host key: %v)\n", err)
		return
	}
	cfg := &ssh.ServerConfig{NoClientAuth: true}
	cfg.AddHostKey(signer)

	ln, err := vsock.Listen(publishedPort, nil)
	if err != nil {
		fmt.Printf("GUEST-RESULT SSHD = FAILED (vsock listen %d: %v)\n", publishedPort, err)
		return
	}
	defer func() { _ = ln.Close() }()

	// The host waits for this line before dialing.
	fmt.Printf("GUEST-RESULT SSHD = LISTENING vsock:%s\n", strconv.Itoa(publishedPort))
	os.Stdout.Sync() //nolint:errcheck // best-effort flush so the host sees readiness promptly

	for {
		c, err := ln.Accept()
		if err != nil {
			fmt.Printf("GUEST: accept: %v\n", err)
			return
		}
		fmt.Println("GUEST-RESULT SSHD = ACCEPTED an inbound connection")
		go serve(c, cfg)
	}
}
