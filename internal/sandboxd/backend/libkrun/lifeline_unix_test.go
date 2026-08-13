//go:build unix

// The descriptor-level and process-level half of the parent-lifeline tests
// (MGIT-103). Unix-tagged because it manipulates real file descriptors and
// builds a three-process tree with exec.Cmd.ExtraFiles, neither of which
// Windows offers — and because the lifeline itself is unix-only, the sandbox
// having no Windows backend in v1.
package libkrun

import (
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/hyper-swe/mgit/internal/model"
)

// lifelinePipe returns a real pipe with tok already written into it, the way
// the daemon primes a VM child's lifeline before exec. Both ends stay owned by
// the test, so it is for cases the code under test must REFUSE — a refusal
// never wraps the descriptor, so no second owner can appear.
func lifelinePipe(t *testing.T, tok string) (r, w *os.File) {
	t.Helper()
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatalf("pipe: %v", err)
	}
	t.Cleanup(func() { _ = r.Close(); _ = w.Close() })
	if tok != "" {
		if _, err := w.WriteString(tok); err != nil {
			t.Fatalf("prime lifeline: %v", err)
		}
	}
	return r, w
}

// rawLifelinePipe is lifelinePipe for the case the code under test ACCEPTS:
// it returns the read end as a bare descriptor, so the os.File that
// lifelineFromEnv builds is the only owner.
//
// Two os.Files over one descriptor is a double-close waiting for a GC, and it
// bit this test before it was written this way: the accepted case left a
// second owner behind, its finalizer closed the descriptor, the number was
// reused by the next test's pipe, and that pipe died mid-test. Which is the
// same class of bug as the one under test — ownership assumed rather than
// established.
func rawLifelinePipe(t *testing.T, tok string) (readFD int, w *os.File) {
	t.Helper()
	var fds [2]int
	if err := syscall.Pipe(fds[:]); err != nil {
		t.Fatalf("pipe: %v", err)
	}
	w = os.NewFile(uintptr(fds[1]), "lifeline-write")
	t.Cleanup(func() { _ = w.Close() })
	if _, err := w.WriteString(tok); err != nil {
		t.Fatalf("prime lifeline: %v", err)
	}
	return fds[0], w
}

// TestLifelineFromEnv_VerifiesProvenance_NeverTrustsTheEnvironment is the
// regression test for the defect that failed CI's Linux Test job. The first
// version accepted any process whose environment named the expected descriptor
// NUMBER, which proves nothing: the variable is inherited by every descendant,
// while the descriptor belongs to whoever happens to hold it. On Linux that
// number is routinely the Go runtime's EPOLL descriptor, so wrapping it in an
// os.File handed its lifetime to the finalizer and the next collection closed
// the netpoller — "epollwait on fd 4 failed with 9 / fatal error: runtime:
// netpoll failed". The quiet failure is worse than the crash: a process that
// reads some other descriptor, sees EOF, and exits announcing that its daemon
// died.
//
// Every case below uses a REAL descriptor rather than a bare number, so the
// test exercises the checks instead of the caller's assumptions — and cannot
// wreck the runtime it runs in. Refs: FR-17.19, MGIT-103
func TestLifelineFromEnv_VerifiesProvenance_NeverTrustsTheEnvironment(t *testing.T) {
	token, err := newLifelineNonce()
	if err != nil {
		t.Fatal(err)
	}
	foreignR, _ := lifelinePipe(t, strings.Repeat("f", len(token)))
	noTokenR, _ := lifelinePipe(t, token)
	silentR, _ := lifelinePipe(t, "") // a pipe nobody ever writes to
	regular, err := os.CreateTemp(t.TempDir(), "notapipe")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = regular.Close() })

	tests := []struct {
		name  string
		fd    string
		token string
		want  bool
	}{
		{name: "unset_means_no_lifeline", fd: "", token: token},
		{name: "not_a_number_is_ignored", fd: "fd", token: token},
		{name: "negative_is_ignored", fd: "-1", token: token},
		{name: "stdio_is_never_a_lifeline", fd: "1", token: token},
		{
			// THE CI FAILURE. A descriptor that is not a pipe is rejected by
			// fstat, before any os.File — and therefore any finalizer — exists
			// for it. The runtime's epoll descriptor is of exactly this shape.
			name:  "a_descriptor_that_is_not_a_pipe_is_refused",
			fd:    strconv.Itoa(int(regular.Fd())),
			token: token,
		},
		{
			// Being a pipe is not enough: another pipe this process happens to
			// hold cannot produce the daemon's nonce.
			name:  "a_pipe_without_the_parents_nonce_is_refused",
			fd:    strconv.Itoa(int(foreignR.Fd())),
			token: token,
		},
		{
			// And a pipe that says nothing is refused rather than left to hang
			// the VM's boot waiting for it.
			name:  "a_silent_pipe_is_refused_not_waited_on_forever",
			fd:    strconv.Itoa(int(silentR.Fd())),
			token: token,
		},
		{
			name:  "no_nonce_in_the_environment_is_refused",
			fd:    strconv.Itoa(int(noTokenR.Fd())),
			token: "",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := lifelineFromEnv(func(k string) string {
				if k == envLifelineNonce {
					return tt.token
				}
				return tt.fd
			})
			if got := f != nil; got != tt.want {
				t.Errorf("lifelineFromEnv(fd=%q) accepted = %v, want %v", tt.fd, got, tt.want)
			}
		})
	}
}

// TestLifelineFromEnv_AcceptsTheParentsPrimedPipe is the positive half: the
// gates must not be so strict that a genuine VM child refuses its own
// lifeline and runs unsupervised — the failure mode that would silently
// restore the leak while every rejection test stayed green.
// Refs: FR-17.19, MGIT-103
func TestLifelineFromEnv_AcceptsTheParentsPrimedPipe(t *testing.T) {
	token, err := newLifelineNonce()
	if err != nil {
		t.Fatal(err)
	}
	readFD, w := rawLifelinePipe(t, token)

	f := lifelineFromEnv(func(k string) string {
		if k == envLifelineNonce {
			return token
		}
		return strconv.Itoa(readFD)
	})
	if f == nil {
		t.Fatal("the daemon's own primed pipe was refused; the VM would run unsupervised")
	}
	t.Cleanup(func() { _ = f.Close() })

	// And it is a working lifeline, not just an accepted one: closing the
	// parent's end is what the watchdog reads as "the daemon is gone".
	_ = w.Close()
	if _, err := io.Copy(io.Discard, f); err != nil {
		t.Errorf("reading the accepted lifeline to EOF: %v", err)
	}
}

// TestLifelineFromEnv_RejectionLeavesTheDescriptorAlone pins the property the
// CI crash turned on: a descriptor mgit decides is NOT its lifeline must come
// back untouched — above all not wrapped in an os.File, whose finalizer closes
// it at the next collection. The descriptors this protects are the Go
// runtime's own, which is why the check runs a GC before looking.
// Refs: MGIT-103
func TestLifelineFromEnv_RejectionLeavesTheDescriptorAlone(t *testing.T) {
	token, err := newLifelineNonce()
	if err != nil {
		t.Fatal(err)
	}
	// A pipe whose nonce will not match: rejected at the last gate, the only
	// one that has to touch the descriptor at all to decide.
	r, w := lifelinePipe(t, strings.Repeat("a", len(token)))

	if f := lifelineFromEnv(func(k string) string {
		if k == envLifelineNonce {
			return token
		}
		return strconv.Itoa(int(r.Fd()))
	}); f != nil {
		t.Fatal("a foreign pipe was accepted as the lifeline")
	}

	// Still usable by its real owner AFTER a collection — the whole point of
	// never letting an os.File finalizer near a descriptor mgit does not own.
	runtime.GC()
	runtime.GC()
	if _, err := w.WriteString("x"); err != nil {
		t.Fatalf("the rejected pipe is no longer writable: %v", err)
	}
	if _, err := r.Read(make([]byte, 1)); err != nil {
		t.Errorf("the rejected pipe is no longer readable after GC: %v", err)
	}
}

// TestReadFDExactly_SilentPipe_TimesOutWithoutTouchingIt covers the bound
// directly: a pipe that never speaks must be given up on, not waited on
// forever, and must not be mutated on the way out. Refs: MGIT-103
func TestReadFDExactly_SilentPipe_TimesOutWithoutTouchingIt(t *testing.T) {
	r, w := lifelinePipe(t, "")
	start := time.Now()
	if _, err := readFDExactly(int(r.Fd()), 8, 150*time.Millisecond); err == nil {
		t.Fatal("a silent pipe produced a token")
	}
	if elapsed := time.Since(start); elapsed > 5*time.Second {
		t.Errorf("waited %s on a silent pipe; a VM boot would hang on it", elapsed)
	}
	// The owner's pipe still works normally.
	if _, err := w.WriteString("still-fine"); err != nil {
		t.Errorf("the pipe was left unusable: %v", err)
	}
}

// envLifelineHelper selects a helper role when the test binary re-execs
// itself for TestSpawnChild_ParentDeathKillsTheVMChild.
const envLifelineHelper = "MGIT_TEST_LIFELINE_HELPER"

// helperReady is the marker the VM-child helper writes to its console once the
// lifeline watchdog is installed and it is parked.
const helperReady = "lifeline-helper-parked"

// helperPark bounds a parked helper so a failed run cannot leave a process
// behind on a developer's machine.
const helperPark = 2 * time.Minute

// helperObserverFD is the extra descriptor the TEST threads down to the VM
// child so it can tell, without ambiguity, when that child has really gone.
// See waitForExit for why a signal-0 probe will not do.
const helperObserverFD = 3

// helperHeld keeps the spawner's lifeline and observer ends reachable at
// package scope. Locals would go unreachable while the spawner parked and
// os.File's finalizer would close them — telling the child its live parent had
// died, and faking the child's own exit. The daemon is safe by construction
// (execChild is reachable from the manager's VM); this helper needs it said.
var helperHeld []*os.File

// runLifelineHelper dispatches the two re-exec roles. It returns false when
// this process is an ordinary test run.
//
//   - "spawn": builds a REAL VM child command with newChildCmd — the derived
//     fd, the nonce primed into the pipe, the parent's retained write end —
//     threads the test's observer descriptor through to it, reports its pid
//     and console path, and parks until it is SIGKILLed.
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
	// The test's observer end rides along AFTER the lifeline, so it cannot
	// disturb the descriptor numbering the lifeline's provenance check
	// depends on. The child never touches it; merely holding it is the point.
	observer := os.NewFile(helperObserverFD, "lifeline-observer")
	c.cmd.ExtraFiles = append(c.cmd.ExtraFiles, observer)
	if err := c.cmd.Start(); err != nil {
		return 1
	}
	c.cleanup()
	// Hand the observer to the child ALONE: while this process holds a copy,
	// the test cannot tell the child's exit from the spawner's.
	_ = observer.Close()
	helperHeld = []*os.File{c.lifeline}
	if _, err := fmt.Printf("vm-pid %d console %s\n", c.cmd.Process.Pid, consolePath); err != nil {
		return 1
	}
	time.Sleep(helperPark)
	return 0
}

// waitForExit blocks until the observer descriptor reports that every process
// holding it has exited, or the deadline passes.
//
// This replaces a signal-0 liveness probe, which is NOT evidence: a process
// that has already exited stays visible as a ZOMBIE until its parent reaps it,
// and a zombie answers signal 0 exactly like a live process. Where the parent
// is dead and PID 1 does not reap (a container, some CI shapes), the zombie is
// permanent — so the probe reports "alive" forever and the test fails a
// working fix, or reports "alive" before the kill and passes a broken one.
// The kernel closes a dead process's descriptors at exit, before reaping, so
// EOF here is unambiguous. Refs: MGIT-103
func waitForExit(observer *os.File, timeout time.Duration) error {
	done := make(chan error, 1)
	go func() {
		_, err := io.Copy(io.Discard, observer)
		done <- err
	}()
	select {
	case err := <-done:
		return err
	case <-time.After(timeout):
		return errors.New("still holding the observer descriptor")
	}
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
	// The observer: the test keeps the read end, the VM child ends up the sole
	// holder of the write end, so EOF means that child has exited — zombie or
	// not, reaped or not.
	observerR, observerW, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = observerR.Close() }()

	spawner := exec.Command(exe) //nolint:gosec,noctx // the test binary itself; killed by this test
	spawner.Env = append(os.Environ(), envLifelineHelper+"=spawn")
	spawner.ExtraFiles = []*os.File{observerW} // helperObserverFD in the spawner
	stdout, err := spawner.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := spawner.Start(); err != nil {
		t.Fatalf("start the lifeline spawner: %v", err)
	}
	_ = observerW.Close() // only the spawned tree may hold it
	defer func() { _ = spawner.Process.Kill(); _ = spawner.Wait() }()

	var vmPID int
	var consolePath string
	if _, err := fmt.Fscanf(stdout, "vm-pid %d console %s\n", &vmPID, &consolePath); err != nil {
		t.Fatalf("the spawner never reported its VM child: %v", err)
	}
	// Running, not merely a pid the kernel still has an entry for: the child
	// wrote this only after installing the watchdog, which also proves the
	// provenance check ACCEPTED the lifeline it was given.
	waitForConsole(t, consolePath, helperReady)

	// The ungraceful exit: SIGKILL leaves no chance to drain — exactly like
	// the crash, the OOM kill and the panic this ticket is about.
	if err := spawner.Process.Kill(); err != nil {
		t.Fatalf("kill the spawner: %v", err)
	}
	_ = spawner.Wait()

	if err := waitForExit(observerR, 20*time.Second); err != nil {
		t.Fatalf("VM child pid %d SURVIVED its parent's SIGKILL (%v) — the orphan is back", vmPID, err)
	}
}
