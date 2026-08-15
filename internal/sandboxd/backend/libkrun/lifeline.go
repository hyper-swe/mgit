package libkrun

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"io"
	"log/slog"
	"os"
	"strconv"
	"time"
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
// lifetime; the child inherits the read end and blocks on it. The kernel
// closes every descriptor of a process that dies, whatever kills it, so EOF
// on that read IS "the daemon is gone" — a guarantee the kernel enforces, not
// a host-side cleanup a SIGKILLed process was never around to run. The child
// then exits, and because libkrun runs the VM INSIDE the child process
// (ADR-010), the VM dies with it.
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
	// envLifelineFD names the descriptor number the parent actually allocated
	// for the lifeline. It is a CLAIM, never proof — see lifelineFromEnv.
	envLifelineFD = "MGIT_VM_LIFELINE_FD"

	// envLifelineNonce carries the nonce the parent wrote INTO the pipe before
	// exec. It is what turns the claim above into evidence: an environment
	// variable is inherited by anything, a nonce on the actual descriptor is
	// not.
	envLifelineNonce = "MGIT_VM_LIFELINE_NONCE"

	// lifelineNonceBytes is the nonce length. 16 random bytes (32 hex chars)
	// is far past any collision concern and stays well inside the atomic
	// single-write guarantee for a pipe (PIPE_BUF), so the child's read can
	// never see a partial token from a live parent.
	lifelineNonceBytes = 16

	// lifelineNonceWait bounds the child's wait for the nonce. The parent
	// writes it BEFORE exec, so a genuine child finds it already buffered and
	// never waits at all; the bound exists so a stray descriptor that happens
	// to be an idle pipe cannot hang a VM's boot instead of being rejected.
	lifelineNonceWait = 2 * time.Second

	// parentGoneExit is the VM child's exit code when its daemon died. It
	// avoids 125-127, which libkrun's in-guest init reserves, so the code can
	// never be mistaken for a guest workload's exit.
	parentGoneExit = 3
)

// newLifelineNonce mints one lifeline nonce.
func newLifelineNonce() (string, error) {
	buf := make([]byte, lifelineNonceBytes)
	if _, err := rand.Read(buf); err != nil {
		return "", fmt.Errorf("vm lifeline token: %w", err)
	}
	return hex.EncodeToString(buf), nil
}

// installParentLifeline starts the child-side watchdog when the parent handed
// this process a lifeline, and does nothing otherwise. It runs before anything
// else in the child: a VM that boots is already worth reaping, and a child
// that has not yet booted one costs nothing to end.
//
// It is called from ChildMain and nowhere else, so only the __krun-vm
// subcommand ever considers a lifeline at all — the first of the three gates
// described on lifelineFromEnv. Refs: FR-17.19, MGIT-103
func installParentLifeline(lookup func(string) string, stderr io.Writer) {
	f := lifelineFromEnv(lookup)
	if f == nil {
		return
	}
	logger := slog.New(slog.NewJSONHandler(stderr, nil))
	go watchLifeline(f, logger, func() { os.Exit(parentGoneExit) })
}

// lifelineFromEnv resolves the child's lifeline read end, or nil when this
// process was not given one.
//
// PROVENANCE IS VERIFIED, NOT INFERRED — this function is the whole reason
// MGIT-103's first attempt was rejected. It originally accepted any process
// whose environment named the expected descriptor number, which proves
// nothing: an environment variable is inherited by every descendant, while
// the descriptor it names belongs to whoever happens to hold it. On Linux the
// number in question is routinely the Go runtime's own EPOLL descriptor, so
// wrapping it in an os.File handed its lifetime to the garbage collector's
// finalizer, and the next collection closed the netpoller out from under the
// runtime: "epollwait on fd 4 failed with 9 / fatal error: runtime: netpoll
// failed". Worse than the crash is the quiet case — a process that reads a
// descriptor which is not the lifeline, sees EOF, and exits 3 announcing that
// its daemon died. A false "my daemon is gone" is a self-inflicted copy of the
// very leak this ticket set out to close.
//
// So three independent gates, none of which trusts the environment:
//
//  1. Only ChildMain calls this at all, so no process but the __krun-vm child
//     ever looks at the descriptor.
//  2. fstat says it is a PIPE. Raw syscall, deliberately before any os.File
//     exists — that ordering is what keeps a wrong descriptor from acquiring a
//     finalizer that would close it. This alone rejects the runtime's epoll
//     descriptor, whose fstat reports no file type at all.
//  3. The parent's NONCE is on it. The daemon writes a fresh random token into
//     the pipe before exec; a descriptor that cannot produce it is not this
//     child's lifeline, whatever the environment says.
//
// The number itself is not hardcoded here either: the parent passes the fd it
// actually allocated, and the only structural requirement is that it is not
// stdio. Refs: FR-17.19, MGIT-103
func lifelineFromEnv(lookup func(string) string) *os.File {
	fd, err := strconv.Atoi(lookup(envLifelineFD))
	if err != nil || fd <= 2 { // stdio is never a lifeline
		return nil
	}
	token := lookup(envLifelineNonce)
	if len(token) != hex.EncodedLen(lifelineNonceBytes) {
		return nil
	}
	// Gate 2, before an os.File can exist for this descriptor.
	if !fdIsPipe(fd) {
		return nil
	}
	// Gate 3: the nonce, read raw for the same reason.
	got, err := readFDExactly(fd, len(token), lifelineNonceWait)
	if err != nil || string(got) != token {
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
