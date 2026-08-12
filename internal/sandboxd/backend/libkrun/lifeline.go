package libkrun

import (
	"io"
	"log/slog"
	"os"
	"strconv"
)

// The PARENT LIFELINE: a microVM must not outlive the daemon that supervises
// it (MGIT-103).
//
// Ordinary daemon exits — idle timeout, SIGINT, SIGTERM — run Daemon.drain,
// which stops and removes every sandbox. The ungraceful ones do not: a
// SIGKILL, an OOM kill or a crash simply ends the process, and every VM child
// was reparented to init and kept running, holding its memory, its share of
// the worktree and its per-VM sockets. Nothing supervised them and no later
// daemon could address them — the only recovery was a manual kill. Measured
// on macOS/libkrun before this fix: `kill -9` of the daemon left the
// `__krun-vm` child alive at 54 MB RSS with the staged worktree still mounted.
//
// THE MECHANISM. The parent holds the write end of a pipe for the VM's
// lifetime; the child inherits the read end at lifelineFD and blocks on it.
// The kernel closes every descriptor of a process that dies, whatever kills
// it, so EOF on that read IS "the daemon is gone" — a guarantee the kernel
// enforces, not a host-side cleanup a SIGKILLed process was never around to
// run. The child then exits, and because libkrun runs the VM INSIDE the child
// process (ADR-010), the VM dies with it.
//
// WHY NOT Pdeathsig. It is Linux-only, and libkrun is the GA backend on
// macOS, which has no equivalent. It also fires on the parent THREAD's death,
// not the parent process's, which in Go means the spawning goroutine must own
// its OS thread for the child's whole life or the signal can arrive early.
// The lifeline has neither problem and is identical on both platforms, so
// there is one mechanism to reason about and one to verify. (The firecracker
// backend cannot use it — its VMM is a foreign binary that does not watch a
// descriptor — and uses Pdeathsig with a pinned thread instead.)
//
// Refs: FR-17.19, FR-17.16, NFR-17.6, MGIT-103, ADR-010
const (
	// lifelineFD is the descriptor the VM child receives the lifeline on:
	// fd 4, immediately after the handshake pipe on fd 3.
	lifelineFD = 4

	// envLifelineFD names the lifeline descriptor to the child. The fd is
	// used ONLY when the parent says so: in a process not spawned by the
	// daemon (run by hand, or by a future caller) fd 4 can be a live Go
	// runtime descriptor, and keying VM teardown on reading it would be its
	// own defect — the same hazard handshakeFD documents.
	envLifelineFD = "MGIT_VM_LIFELINE_FD"

	// parentGoneExit is the VM child's exit code when its daemon died. It
	// avoids 125-127, which libkrun's in-guest init reserves, so the code can
	// never be mistaken for a guest workload's exit.
	parentGoneExit = 3
)

// installParentLifeline starts the child-side watchdog when the parent named a
// lifeline descriptor, and does nothing otherwise. It runs before anything
// else in the child: a VM that boots is already worth reaping, and a child
// that has not yet booted one costs nothing to end.
// Refs: FR-17.19, MGIT-103
func installParentLifeline(lookup func(string) string, stderr io.Writer) {
	f := lifelineFromEnv(lookup)
	if f == nil {
		return
	}
	logger := slog.New(slog.NewJSONHandler(stderr, nil))
	go watchLifeline(f, logger, func() { os.Exit(parentGoneExit) })
}

// lifelineFromEnv resolves the child's lifeline read end from the environment,
// or nil when this process was not given one. Only the descriptor the parent
// actually passes is accepted: stdio and the handshake fd are never lifelines,
// and a malformed value is treated as "no lifeline" rather than guessed at.
// Refs: MGIT-103
func lifelineFromEnv(lookup func(string) string) *os.File {
	fd, err := strconv.Atoi(lookup(envLifelineFD))
	if err != nil || fd != lifelineFD {
		return nil
	}
	return os.NewFile(uintptr(fd), "mgit-vm-lifeline")
}

// watchLifeline blocks until the lifeline reports that the parent is gone,
// then calls halt. Bytes on it carry no meaning — the lifeline is a liveness
// channel, and only its END is the signal — so they are drained and ignored.
//
// A read that FAILS also halts. That is deliberate: a lifeline that cannot be
// read is not a lifeline, and a VM nobody can supervise must end rather than
// persist unsupervised. The alternative (keep running on a broken channel) is
// the leak this ticket exists to close.
// Refs: FR-17.19, MGIT-103
func watchLifeline(r io.Reader, logger *slog.Logger, halt func()) {
	_, err := io.Copy(io.Discard, r)
	logger.Error("libkrun vm halting: its daemon is gone",
		"event", "krun_vm_parent_gone", "reason", lifelineReason(err))
	halt()
}

// lifelineReason renders why the lifeline ended, so the per-VM console log
// distinguishes "the daemon died" from "the lifeline broke".
func lifelineReason(err error) string {
	if err == nil {
		return "lifeline closed (parent process exited)"
	}
	return "lifeline unreadable: " + err.Error()
}
