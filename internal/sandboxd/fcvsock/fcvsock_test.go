package fcvsock

import (
	"bufio"
	"context"
	"io"
	"net"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// shortSocketPath returns a unix socket path short enough for the OS
// sun_path limit (~104 bytes on darwin); t.TempDir() paths are too long.
func shortSocketPath(t *testing.T) string {
	t.Helper()
	dir, err := os.MkdirTemp("/tmp", "fcv")
	require.NoError(t, err)
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	return filepath.Join(dir, "s.sock")
}

// fakeFirecracker is a unix-socket server that mimics the firecracker
// host-initiated vsock handshake: it reads one "CONNECT <port>\n" line and
// replies with reply, then runs after(conn) for post-handshake behavior.
type fakeFirecracker struct {
	reply string
	after func(conn net.Conn)
	got   chan string
}

func startFakeFirecracker(t *testing.T, f *fakeFirecracker) string {
	t.Helper()
	udsPath := shortSocketPath(t)
	ln, err := net.Listen("unix", udsPath)
	require.NoError(t, err)
	f.got = make(chan string, 1)
	t.Cleanup(func() { _ = ln.Close() })

	go func() {
		conn, err := ln.Accept()
		if err != nil {
			return
		}
		defer func() { _ = conn.Close() }()
		line, _ := bufio.NewReader(conn).ReadString('\n')
		f.got <- line
		if f.reply != "" {
			_, _ = io.WriteString(conn, f.reply)
		}
		if f.after != nil {
			f.after(conn)
		}
	}()
	return udsPath
}

// TestFCVsock_Dial_HandshakeSucceeds verifies the CONNECT handshake is
// sent for the requested port and an "OK ..." reply yields a usable conn.
func TestFCVsock_Dial_HandshakeSucceeds(t *testing.T) {
	f := &fakeFirecracker{reply: "OK 12345\n"}
	uds := startFakeFirecracker(t, f)

	conn, err := Dial(context.Background(), uds, 1024)
	require.NoError(t, err)
	defer func() { _ = conn.Close() }()

	select {
	case got := <-f.got:
		assert.Equal(t, "CONNECT 1024\n", got, "the guest port is requested via CONNECT")
	case <-time.After(2 * time.Second):
		t.Fatal("server never received the CONNECT line")
	}
}

// TestFCVsock_Dial_StreamUsableAfterHandshake verifies the handshake does
// not consume guest-stream bytes that immediately follow the reply line.
func TestFCVsock_Dial_StreamUsableAfterHandshake(t *testing.T) {
	f := &fakeFirecracker{
		reply: "OK 1\n",
		after: func(conn net.Conn) { _, _ = io.WriteString(conn, "HELLO-FROM-GUEST") },
	}
	uds := startFakeFirecracker(t, f)

	conn, err := Dial(context.Background(), uds, 1024)
	require.NoError(t, err)
	defer func() { _ = conn.Close() }()

	buf := make([]byte, len("HELLO-FROM-GUEST"))
	_, err = io.ReadFull(conn, buf)
	require.NoError(t, err)
	assert.Equal(t, "HELLO-FROM-GUEST", string(buf), "no post-handshake stream bytes are lost")
}

// TestFCVsock_Dial_HandshakeRejected verifies a non-OK reply fails closed.
func TestFCVsock_Dial_HandshakeRejected(t *testing.T) {
	f := &fakeFirecracker{reply: "ERROR no such port\n"}
	uds := startFakeFirecracker(t, f)

	conn, err := Dial(context.Background(), uds, 1024)
	require.Error(t, err)
	assert.Nil(t, conn)
	assert.Contains(t, err.Error(), "rejected")
}

// TestFCVsock_Dial_SocketAbsent verifies dialing a missing socket fails.
func TestFCVsock_Dial_SocketAbsent(t *testing.T) {
	conn, err := Dial(context.Background(), shortSocketPath(t), 1024) // dir exists, socket never created
	require.Error(t, err)
	assert.Nil(t, conn)
}

// TestFCVsock_Dial_ServerClosesBeforeReply verifies a closed connection
// during the handshake (EOF before a newline) is an error.
func TestFCVsock_Dial_ServerClosesBeforeReply(t *testing.T) {
	f := &fakeFirecracker{reply: ""} // accept, read CONNECT, then close with no reply
	uds := startFakeFirecracker(t, f)

	conn, err := Dial(context.Background(), uds, 1024)
	require.Error(t, err)
	assert.Nil(t, conn)
}

// TestFCVsock_Dial_OversizedReplyRejected verifies a reply with no newline
// within the cap fails rather than reading unbounded.
func TestFCVsock_Dial_OversizedReplyRejected(t *testing.T) {
	long := make([]byte, maxHandshakeBytes+50)
	for i := range long {
		long[i] = 'x' // no newline, never "OK ", exceeds the cap
	}
	f := &fakeFirecracker{reply: string(long)}
	uds := startFakeFirecracker(t, f)

	conn, err := Dial(context.Background(), uds, 1024)
	require.Error(t, err)
	assert.Nil(t, conn)
	assert.Contains(t, err.Error(), "exceeds")
}
