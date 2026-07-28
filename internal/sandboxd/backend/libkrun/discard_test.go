package libkrun

import (
	"errors"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// "none" mode cannot be expressed as "a socket path nothing serves": libkrun
// accepts the add (krun_add_net_unixgram returns 0 either way) but the VM then
// HANGS at boot, never reaching the guest — measured on libkrun 1.19.4, see
// ADR-010. A deny backing must therefore be a real bound unixgram socket that
// discards everything, held for the VM's lifetime and removed at teardown.
// Refs: FR-17.7, SEC-04, ADR-010

// shortTempDir is used instead of t.TempDir(): a unix socket path must fit in
// sun_path (104 bytes on darwin), and t.TempDir() embeds the test name, which
// overflows it for these test names.
func shortTempDir(t *testing.T) string {
	t.Helper()
	dir, err := os.MkdirTemp("", "lk")
	if err != nil {
		t.Fatalf("temp dir: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	return dir
}

// mustBindDiscard binds a deny socket and closes it at test end.
func mustBindDiscard(t *testing.T, path string) *discardSocket {
	t.Helper()
	d, err := bindDiscardSocket(path)
	if err != nil {
		t.Fatalf("bindDiscardSocket: %v", err)
	}
	t.Cleanup(func() { _ = d.Close() })
	return d
}

func TestDiscardSocket_BindsAndIsConnectable(t *testing.T) {
	path := filepath.Join(shortTempDir(t), "net-deny.sock")

	d := mustBindDiscard(t, path)
	_ = d

	if _, err := os.Stat(path); err != nil {
		t.Fatalf("socket not present on disk: %v", err)
	}

	// The VMM must be able to send to it without blocking or erroring; a
	// path-only deny is what hangs the VM.
	c, err := net.Dial("unixgram", path)
	if err != nil {
		t.Fatalf("dial discard socket: %v", err)
	}
	defer func() { _ = c.Close() }()
	if _, err := c.Write([]byte("frame")); err != nil {
		t.Errorf("write to discard socket: %v", err)
	}
}

func TestDiscardSocket_KeepsDrainingSoTheGuestIsNeverWedged(t *testing.T) {
	path := filepath.Join(shortTempDir(t), "net-deny.sock")
	d := mustBindDiscard(t, path)
	_ = d

	c, err := net.Dial("unixgram", path)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer func() { _ = c.Close() }()

	// Datagram sockets have no flow control, so a burst can transiently hit
	// ENOBUFS even with a reader attached. The property that matters is that
	// the socket never wedges PERMANENTLY: the drain must keep making room, or
	// the guest's NIC backs up and the VM stalls the way a path-only deny does.
	const want = 2000
	payload := make([]byte, 1024)
	deadline := time.Now().Add(10 * time.Second)
	sent := 0
	for sent < want && time.Now().Before(deadline) {
		if _, err := c.Write(payload); err != nil {
			time.Sleep(time.Millisecond) // let the drain catch up, then retry
			continue
		}
		sent++
	}
	if sent < want {
		t.Fatalf("only %d/%d datagrams accepted before the deadline — "+
			"the discard socket is not draining", sent, want)
	}
}

func TestDiscardSocket_Close_RemovesTheSocketFile(t *testing.T) {
	path := filepath.Join(shortTempDir(t), "net-deny.sock")
	d, err := bindDiscardSocket(path)
	if err != nil {
		t.Fatalf("bindDiscardSocket: %v", err)
	}
	if err := d.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	// Teardown must leave no residue (SEC-10).
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Errorf("socket file still present after Close (err=%v)", err)
	}
	// Close is idempotent: teardown paths may run twice.
	if err := d.Close(); err != nil {
		t.Errorf("second Close should be a no-op, got %v", err)
	}
}

func TestDiscardSocket_StaleSocketFile_IsReplaced(t *testing.T) {
	dir := shortTempDir(t)
	path := filepath.Join(dir, "net-deny.sock")
	// A crashed daemon can leave the socket behind; relaunch must not fail.
	if err := os.WriteFile(path, []byte("stale"), 0o600); err != nil {
		t.Fatalf("seed stale file: %v", err)
	}

	_ = mustBindDiscard(t, path)

	probe, err := net.Dial("unixgram", path)
	if err != nil {
		t.Errorf("stale file was not replaced by a live socket: %v", err)
		return
	}
	_ = probe.Close()
}

func TestDiscardSocket_UnbindablePath_Errors(t *testing.T) {
	// A path under a nonexistent directory cannot be bound.
	if _, err := bindDiscardSocket(filepath.Join(shortTempDir(t), "nope", "net-deny.sock")); err == nil {
		t.Fatal("expected an error binding into a nonexistent directory")
	}
}

// failingPeer is a host peer whose teardown fails, so guestCtx.Close's
// error-joining path is exercised.
type failingPeer struct{ err error }

func (f failingPeer) Close() error { return f.err }

func TestGuestCtx_Close_ReportsAHostPeerTeardownFailure(t *testing.T) {
	api := &fakeKrun{}
	// Constructed directly (not via newGuestCtx) to inject a failing peer;
	// the AST guards deliberately scan only non-test files.
	gc := &guestCtx{api: api, id: 1, peer: failingPeer{err: errors.New("socket stuck")}}

	err := gc.Close()
	if err == nil {
		t.Fatal("expected an error when the host peer fails to close")
	}
	if !strings.Contains(err.Error(), "close host net peer") {
		t.Errorf("error %q does not name the failing resource", err)
	}
	// The libkrun context must still be released even when the peer fails.
	if !strings.Contains(api.seq(), "free_ctx") {
		t.Errorf("context not freed when the peer failed: %q", api.seq())
	}
}

func TestBindDiscardSocket_StalePathIsANonEmptyDir_Errors(t *testing.T) {
	path := filepath.Join(shortTempDir(t), denySocketName)
	// A directory with contents cannot be cleared by os.Remove, so the
	// stale-path branch must surface rather than fall through to bind.
	if err := os.MkdirAll(filepath.Join(path, "child"), 0o700); err != nil {
		t.Fatalf("seed dir: %v", err)
	}
	if _, err := bindDiscardSocket(path); err == nil {
		t.Fatal("expected an error clearing an unremovable stale path")
	}
}

func TestDiscardSocket_Close_UnremovableSocket_ReportsError(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root ignores directory permissions")
	}
	dir := shortTempDir(t)
	path := filepath.Join(dir, denySocketName)
	d, err := bindDiscardSocket(path)
	if err != nil {
		t.Fatalf("bindDiscardSocket: %v", err)
	}
	// Make the socket unremovable so teardown cannot leave a clean state.
	//nolint:gosec // G302 targets files; a directory needs its traverse bit,
	// and 0500 is the point of the test (make the socket unremovable).
	if err := os.Chmod(dir, 0o500); err != nil {
		t.Fatalf("chmod: %v", err)
	}
	//nolint:gosec // G302: restoring owner-only rwx on a directory so cleanup can run.
	t.Cleanup(func() { _ = os.Chmod(dir, 0o700) })

	if err := d.Close(); err == nil {
		t.Fatal("expected an error when the socket file cannot be removed")
	}
}
