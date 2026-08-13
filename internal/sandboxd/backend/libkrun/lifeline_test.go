package libkrun

import (
	"bytes"
	"errors"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/hyper-swe/mgit/internal/model"
)

// errReader fails every read, modeling a lifeline descriptor that cannot be
// read at all (already closed, or never a pipe).
type errReader struct{ err error }

func (r errReader) Read([]byte) (int, error) { return 0, r.err }

// blockingReader never returns, modeling a live parent holding its end open.
type blockingReader struct{ release chan struct{} }

func (r blockingReader) Read([]byte) (int, error) {
	<-r.release
	return 0, io.EOF
}

// TestWatchLifeline_ParentEndClosed_HaltsTheChild pins the mechanism this
// ticket exists for: the kernel closes every descriptor of a process that
// dies, however it dies, so EOF on the lifeline IS "the daemon is gone" — and
// a VM child that learns that must not keep running unsupervised.
// Refs: FR-17.19, MGIT-103
func TestWatchLifeline_ParentEndClosed_HaltsTheChild(t *testing.T) {
	tests := []struct {
		name   string
		reader io.Reader
	}{
		{
			// The real shape: the parent's write end closes, the child's read
			// end reports EOF.
			name:   "eof_is_the_parent_dying",
			reader: strings.NewReader(""),
		},
		{
			// Bytes are not a protocol: whatever arrives, the lifeline is only
			// meaningful when it ENDS. Drain, then halt.
			name:   "bytes_then_eof_still_halts",
			reader: strings.NewReader("noise"),
		},
		{
			// Fail closed: a lifeline that cannot be read is not a lifeline,
			// and a VM nobody can supervise must end rather than persist.
			name:   "unreadable_lifeline_fails_closed",
			reader: errReader{err: errors.New("bad file descriptor")},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var logs bytes.Buffer
			halted := make(chan struct{})
			go watchLifeline(tt.reader, slog.New(slog.NewJSONHandler(&logs, nil)),
				func() { close(halted) })
			select {
			case <-halted:
			case <-time.After(5 * time.Second):
				t.Fatal("the child was never halted; an orphaned VM would keep running")
			}
			if !strings.Contains(logs.String(), "krun_vm_parent_gone") {
				t.Errorf("log = %q, want the krun_vm_parent_gone record so the console log says WHY the VM ended",
					logs.String())
			}
		})
	}
}

// TestWatchLifeline_ParentAlive_DoesNotHalt is the other half: a VM must not
// be torn down while its daemon is alive. A reaping mechanism that fires early
// would trade a rare leak for a common outage.
// Refs: MGIT-103
func TestWatchLifeline_ParentAlive_DoesNotHalt(t *testing.T) {
	release := make(chan struct{})
	defer close(release)
	halted := make(chan struct{})
	go watchLifeline(blockingReader{release: release},
		slog.New(slog.NewJSONHandler(io.Discard, nil)), func() { close(halted) })
	select {
	case <-halted:
		t.Fatal("the child halted while its parent was still holding the lifeline")
	case <-time.After(200 * time.Millisecond):
	}
}

// TestInstallParentLifeline_WithoutTheEnvironment_IsANoOp covers the guard on
// the installer itself: a child run by hand — or any future caller that
// re-execs ChildMain without the daemon's plumbing — must not end up with a
// watchdog reading a descriptor that means nothing, whose EOF would halt the
// process. Refs: MGIT-103
func TestInstallParentLifeline_WithoutTheEnvironment_IsANoOp(t *testing.T) {
	var stderr bytes.Buffer
	installParentLifeline(func(string) string { return "" }, &stderr)
	// Nothing was installed, so nothing can report a gone parent. Give a
	// wrongly-installed watchdog time to fire before concluding it.
	time.Sleep(200 * time.Millisecond)
	if strings.Contains(stderr.String(), "krun_vm_parent_gone") {
		t.Errorf("a child with no lifeline installed a watchdog anyway: %q", stderr.String())
	}
}

// TestNewChildCmd_WiresTheParentLifeline asserts the plumbing a VM child needs
// to notice its daemon died: the read end as an extra descriptor, the fd
// number AND the identifying nonce in the child's environment, and the
// parent's write end handed back to be held for the VM's lifetime. Without
// that last one the lifeline would close as soon as the spawn returned and
// every VM would die at boot. Refs: MGIT-103
func TestNewChildCmd_WiresTheParentLifeline(t *testing.T) {
	dir := shortTempDir(t)
	spec := baseSpec(model.NetworkModeNone, dir)

	c, err := newChildCmd("/fake/mgit-sandboxd", spec, filepath.Join(dir, consoleLogName))
	if err != nil {
		t.Fatalf("newChildCmd: %v", err)
	}
	t.Cleanup(func() { c.cleanup(); _ = c.handshake.Close(); _ = c.lifeline.Close() })

	if len(c.cmd.ExtraFiles) != 2 {
		t.Fatalf("ExtraFiles = %d, want the handshake pipe and the parent lifeline",
			len(c.cmd.ExtraFiles))
	}
	if c.lifeline == nil {
		t.Fatal("no parent lifeline end returned; nothing would hold it open and the VM would die at boot")
	}

	env := map[string]string{}
	for _, kv := range c.cmd.Env {
		if k, v, ok := strings.Cut(kv, "="); ok {
			env[k] = v
		}
	}
	// The fd is DERIVED from the extra-file position, not hardcoded, so the
	// number the child is told stays the number it is given.
	if want := strconv.Itoa(extraFileFD(1)); env[envLifelineFD] != want {
		t.Errorf("%s = %q, want %q (the lifeline's actual extra-file slot)",
			envLifelineFD, env[envLifelineFD], want)
	}
	// The nonce is what makes the fd number evidence rather than a claim.
	if got := env[envLifelineNonce]; len(got) != 2*lifelineNonceBytes {
		t.Errorf("%s = %q, want a %d-char nonce", envLifelineNonce, got, 2*lifelineNonceBytes)
	}

	// cleanup closes the parent's copies of the CHILD's files. It must not
	// touch the lifeline WRITE end: that one is the supervision link, and
	// closing it would tell a child with a live parent that its parent had
	// died. (Nothing was spawned here, so the write itself fails EPIPE for
	// want of a reader; what must not happen is the descriptor being closed.)
	c.cleanup()
	_, err = c.lifeline.Write([]byte{0})
	if errors.Is(err, os.ErrClosed) {
		t.Error("cleanup closed the lifeline write end; every VM would be told its daemon had died")
	}
}

// TestNewChildCmd_PrimesTheLifelineBeforeExec asserts the nonce is already in
// the pipe when the command is handed back. It must be written BEFORE the
// child execs — a child that had to wait for it would stall its own boot, and
// a child that never received it would refuse the lifeline and run
// unsupervised. Refs: MGIT-103
func TestNewChildCmd_PrimesTheLifelineBeforeExec(t *testing.T) {
	dir := shortTempDir(t)
	c, err := newChildCmd("/fake/mgit-sandboxd", baseSpec(model.NetworkModeNone, dir),
		filepath.Join(dir, consoleLogName))
	if err != nil {
		t.Fatalf("newChildCmd: %v", err)
	}
	t.Cleanup(func() { c.cleanup(); _ = c.handshake.Close(); _ = c.lifeline.Close() })

	var token string
	for _, kv := range c.cmd.Env {
		if v, ok := strings.CutPrefix(kv, envLifelineNonce+"="); ok {
			token = v
		}
	}
	if token == "" {
		t.Fatal("no lifeline nonce in the child environment")
	}
	// The child's read end is the extra file; read what the parent left there.
	got := make([]byte, len(token))
	if _, err := io.ReadFull(c.cmd.ExtraFiles[1], got); err != nil {
		t.Fatalf("read the primed nonce: %v", err)
	}
	if string(got) != token {
		t.Errorf("pipe carries %q, environment announces %q — the child would refuse its own lifeline",
			got, token)
	}
}
