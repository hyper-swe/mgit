package libkrun

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
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

// TestLifelineFromEnv_OnlyWhenTheParentNamesTheFD guards the same hazard the
// handshake fd carries: in a process NOT spawned by the daemon (run by hand,
// or by a future caller), fd 4 can be a live Go runtime descriptor, and
// keying VM teardown on reading it would be its own defect. The lifeline is
// used only when the parent explicitly names it. Refs: MGIT-103
func TestLifelineFromEnv_OnlyWhenTheParentNamesTheFD(t *testing.T) {
	tests := []struct {
		name  string
		value string
		want  bool
	}{
		{name: "unset_means_no_lifeline", value: "", want: false},
		{name: "not_a_number_is_ignored", value: "fd", want: false},
		{name: "stdio_is_never_a_lifeline", value: "1", want: false},
		{name: "handshake_fd_is_never_a_lifeline", value: strconv.Itoa(handshakeFD), want: false},
		{name: "negative_is_ignored", value: "-1", want: false},
		{name: "the_fd_the_parent_passes", value: strconv.Itoa(lifelineFD), want: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := lifelineFromEnv(func(string) string { return tt.value })
			if got := f != nil; got != tt.want {
				t.Errorf("lifelineFromEnv(%q) non-nil = %v, want %v", tt.value, got, tt.want)
			}
		})
	}
}

// TestNewChildCmd_WiresTheParentLifeline asserts the plumbing a VM child needs
// to notice its daemon died: the read end at fd 4, that fd named in the
// child's environment, and the parent's write end handed back to be held for
// the VM's lifetime. Without the last one the lifeline would close as soon as
// the spawn returned and every VM would die at boot. Refs: MGIT-103
func TestNewChildCmd_WiresTheParentLifeline(t *testing.T) {
	dir := shortTempDir(t)
	spec := baseSpec(model.NetworkModeNone, dir)

	c, err := newChildCmd("/fake/mgit-sandboxd", spec, filepath.Join(dir, consoleLogName))
	if err != nil {
		t.Fatalf("newChildCmd: %v", err)
	}
	t.Cleanup(func() { c.cleanup(); _ = c.handshake.Close(); _ = c.lifeline.Close() })

	if len(c.cmd.ExtraFiles) != 2 {
		t.Fatalf("ExtraFiles = %d, want the handshake pipe (fd 3) and the lifeline (fd 4)",
			len(c.cmd.ExtraFiles))
	}
	if c.lifeline == nil {
		t.Fatal("no parent lifeline end returned; nothing would hold it open and the VM would die at boot")
	}
	want := envLifelineFD + "=" + strconv.Itoa(lifelineFD)
	var found bool
	for _, kv := range c.cmd.Env {
		if kv == want {
			found = true
		}
	}
	if !found {
		t.Errorf("child env = %v, want %q so the child reads the lifeline it was actually given",
			c.cmd.Env, want)
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

// TestInstallParentLifeline_WithoutTheFDVar_IsANoOp covers the guard on the
// installer itself, not just its resolver: a child run by hand — or any future
// caller that re-execs ChildMain without the daemon's plumbing — must not end
// up with a watchdog reading a descriptor that means nothing, whose EOF would
// halt the process. Refs: MGIT-103
func TestInstallParentLifeline_WithoutTheFDVar_IsANoOp(t *testing.T) {
	var stderr bytes.Buffer
	installParentLifeline(func(string) string { return "" }, &stderr)
	// No watchdog was installed, so nothing can report a gone parent. Give a
	// wrongly-installed one time to fire before concluding it.
	time.Sleep(200 * time.Millisecond)
	if strings.Contains(stderr.String(), "krun_vm_parent_gone") {
		t.Errorf("a child with no lifeline installed a watchdog anyway: %q", stderr.String())
	}
}

// envLifelineHelper selects a helper role when the test binary re-execs
// itself for TestSpawnChild_ParentDeathKillsTheVMChild.
const envLifelineHelper = "MGIT_TEST_LIFELINE_HELPER"

// helperLifeline holds the spawner's end of the lifeline at package scope.
// A local variable would become unreachable while the spawner parked, and
// os.File's finalizer would close the descriptor — telling the child its
// live parent had died. The daemon is safe by construction (execChild is
// reachable from the manager's VM), but this helper needs it stated.
var helperLifeline *os.File

// helperReady is the marker the VM-child helper writes to its console once
// the lifeline watchdog is installed and it is parked. The test waits for it:
// a process that has ALREADY exited is a zombie until its parent reaps it, and
// a zombie answers signal 0 like a live process — so "the pid responds" is not
// evidence the child was running, and a test built on it passes without the
// mechanism under test. (It did, until this marker was added.)
const helperReady = "lifeline-helper-parked"

// runLifelineHelper dispatches the two re-exec roles. It returns false when
// this process is an ordinary test run.
//
//   - "spawn": builds a REAL VM child command with newChildCmd (fd 4, the env
//     var, the parent's retained write end), starts it, reports its pid and
//     console path, and parks until it is SIGKILLed.
//   - "child": installs the REAL child-side lifeline watchdog, reports ready
//     on its console, and parks. Only the libkrun body is absent — a test
//     build has no bootable VM to put there.
func runLifelineHelper() (int, bool) {
	switch os.Getenv(envLifelineHelper) {
	case "spawn":
		return lifelineSpawnHelper(), true
	case "child":
		installParentLifeline(os.Getenv, os.Stderr)
		if _, err := fmt.Fprintln(os.Stderr, helperReady); err != nil {
			return 1, true
		}
		time.Sleep(helperPark)
		return 0, true // parked out: the lifeline never fired
	}
	return 0, false
}

// helperPark bounds a parked helper so a failed run cannot leave a process
// behind on a developer's machine.
const helperPark = 2 * time.Minute

func lifelineSpawnHelper() int {
	exe, err := os.Executable()
	if err != nil {
		return 1
	}
	dir, err := os.MkdirTemp("", "lk")
	if err != nil {
		return 1
	}
	consolePath := filepath.Join(dir, consoleLogName)
	c, err := newChildCmd(exe, baseSpec(model.NetworkModeNone, dir), consolePath)
	if err != nil {
		return 1
	}
	// The child takes the helper role rather than ChildMain: a test build has
	// no bootable VM, so a real VM child would exit before it could be
	// orphaned. Everything else about the spawn is the production path.
	c.cmd.Env = append(c.cmd.Env, envLifelineHelper+"=child")
	if err := c.cmd.Start(); err != nil {
		return 1
	}
	c.cleanup()
	helperLifeline = c.lifeline
	if _, err := fmt.Printf("vm-pid %d console %s\n", c.cmd.Process.Pid, consolePath); err != nil {
		return 1
	}
	time.Sleep(helperPark)
	return 0
}

// processAlive reports whether pid names a process the kernel still knows
// about. Signal 0 performs only the permission and existence checks. A ZOMBIE
// answers it too, which is why the caller must first see helperReady.
func processAlive(pid int) bool {
	p, err := os.FindProcess(pid)
	if err != nil {
		return false
	}
	return p.Signal(syscall.Signal(0)) == nil
}

// waitForConsole polls path until it contains want, or fails the test.
func waitForConsole(t *testing.T, path, want string) {
	t.Helper()
	deadline := time.Now().Add(20 * time.Second)
	for {
		b, err := os.ReadFile(path) //nolint:gosec // helper-owned temp path
		if err == nil && strings.Contains(string(b), want) {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("console %s never reported %q (got %q)", path, want, b)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// TestSpawnChild_ParentDeathKillsTheVMChild is MGIT-103's reproduction in
// process form, and the only test here that proves the whole mechanism rather
// than a piece of it: a real child holding a real lifeline, whose real parent
// is SIGKILLed — no drain, no teardown, nothing host-side left to run. It runs
// on macOS and Linux alike, because the guarantee it leans on (the kernel
// closes a dead process's descriptors) is POSIX, not Linux.
// Refs: FR-17.19, MGIT-103
func TestSpawnChild_ParentDeathKillsTheVMChild(t *testing.T) {
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	spawner := exec.Command(exe) //nolint:gosec,noctx // the test binary itself; killed by this test
	spawner.Env = append(os.Environ(), envLifelineHelper+"=spawn")
	stdout, err := spawner.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := spawner.Start(); err != nil {
		t.Fatalf("start the lifeline spawner: %v", err)
	}
	defer func() { _ = spawner.Process.Kill(); _ = spawner.Wait() }()

	var vmPID int
	var consolePath string
	if _, err := fmt.Fscanf(stdout, "vm-pid %d console %s\n", &vmPID, &consolePath); err != nil {
		t.Fatalf("the spawner never reported its VM child: %v", err)
	}
	// Alive AND running, not merely a pid the kernel still has an entry for.
	waitForConsole(t, consolePath, helperReady)
	if !processAlive(vmPID) {
		t.Fatalf("the VM child (pid %d) was not running before the kill", vmPID)
	}

	// The ungraceful exit: SIGKILL leaves no chance to drain — exactly like
	// the crash, the OOM kill and the panic this ticket is about.
	if err := spawner.Process.Kill(); err != nil {
		t.Fatalf("kill the spawner: %v", err)
	}
	_ = spawner.Wait()

	deadline := time.Now().Add(20 * time.Second)
	for processAlive(vmPID) {
		if time.Now().After(deadline) {
			t.Fatalf("VM child pid %d SURVIVED its parent's SIGKILL — the orphan is back", vmPID)
		}
		time.Sleep(20 * time.Millisecond)
	}
}
