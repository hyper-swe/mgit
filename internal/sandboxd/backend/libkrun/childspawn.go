// The VM CHILD SPAWN: building, starting and reaping the re-exec child that
// is one libkrun microVM (ADR-010). Split from hypervisor.go, which owns the
// Hypervisor and VM lifecycle, so neither file outgrows the 500-line limit and
// the process plumbing — stdio contract, extra descriptors, minimal
// environment, parent lifeline — reads in one place. Pure move; the lifeline
// itself lives in lifeline.go. Refs: ADR-010, MGIT-61.8, MGIT-103

package libkrun

import (
	"bytes"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
)

// spawnChild is the real spawnFunc: it re-execs the daemon binary under
// ChildCommand with the spec on stdin, console output captured to a per-VM
// file, and the handshake pipe on fd 3. The daemon's own stdio is NEVER
// inherited — krun_start_enter hands the child's stdin/stdout to the guest,
// and a hostile guest must not hold the daemon's streams. Refs: SEC-10, ADR-010
func spawnChild(exePath string, spec vmSpec, consolePath string) (childProcess, error) {
	c, err := newChildCmd(exePath, spec, consolePath)
	if err != nil {
		return nil, err
	}
	if err := c.cmd.Start(); err != nil {
		c.cleanup()
		_ = c.handshake.Close()
		_ = c.lifeline.Close()
		return nil, fmt.Errorf("start vm child: %w", err)
	}
	// The parent's copies of the child-held files are closed once the child
	// owns them; the read end of the handshake pipe stays — and so does the
	// WRITE end of the lifeline, for the VM's whole life (MGIT-103).
	c.cleanup()
	return &execChild{cmd: c.cmd, handshake: c.handshake, lifeline: c.lifeline}, nil
}

// childCmd is a built-but-unstarted VM child: the command, the parent's ends
// of its two pipes, and the cleanup that closes the parent's copies of the
// files the child takes over.
type childCmd struct {
	cmd       *exec.Cmd
	handshake *os.File // parent's READ end of the fd-3 progress pipe
	lifeline  *os.File // parent's WRITE end of the parent lifeline (MGIT-103)
	cleanup   func()
}

// extraFileFD is the descriptor number a child sees for the nth entry of
// exec.Cmd.ExtraFiles: they land immediately after stdio, in order. Deriving
// it beats hardcoding, so the number the child is TOLD stays the number it is
// actually GIVEN if this list ever grows. Refs: MGIT-103
func extraFileFD(index int) int { return 3 + index }

// newChildCmd builds the child command with its full stdio contract. Split
// from spawnChild so tests can assert the wiring — spec on stdin, console
// file (never the daemon's streams) on stdout/stderr, handshake pipe and
// parent lifeline as the only extra fds — without spawning anything.
//
// The returned cleanup closes the parent's copies of the files handed to the
// child. It deliberately does NOT close the lifeline write end: that one is
// the supervision link the child keys its own teardown on, and closing it
// would tell a child with a live parent that its parent had died.
// Refs: SEC-10, ADR-010, MGIT-103
func newChildCmd(exePath string, spec vmSpec, consolePath string) (*childCmd, error) {
	var specJSON bytes.Buffer
	if err := spec.encode(&specJSON); err != nil {
		return nil, err
	}
	console, err := os.OpenFile(consolePath, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600) //nolint:gosec // path built from the manager-owned state dir
	if err != nil {
		return nil, fmt.Errorf("vm console log: %w", err)
	}
	handshakeR, handshakeW, err := os.Pipe()
	if err != nil {
		_ = console.Close()
		return nil, fmt.Errorf("vm handshake pipe: %w", err)
	}
	// The parent lifeline: the child holds the read end, the daemon the write
	// end. Its EOF is the daemon's death, however the daemon dies. Refs: MGIT-103
	lifelineR, lifelineW, err := os.Pipe()
	if err != nil {
		_ = console.Close()
		_ = handshakeR.Close()
		_ = handshakeW.Close()
		return nil, fmt.Errorf("vm lifeline pipe: %w", err)
	}

	cleanup := func() {
		_ = console.Close()
		_ = handshakeW.Close()
		_ = lifelineR.Close()
	}

	// The nonce goes into the pipe BEFORE exec, so the child finds it already
	// buffered and never waits. It is what lets the child prove the descriptor
	// it was told about is really the one the daemon opened, rather than
	// trusting an environment variable that anything can inherit (MGIT-103).
	// A lifeline that cannot be identified fails the spawn: better no VM than
	// an unsupervised one.
	token, err := newLifelineNonce()
	if err != nil {
		cleanup()
		_ = handshakeR.Close()
		_ = lifelineW.Close()
		return nil, err
	}
	if _, err := lifelineW.WriteString(token); err != nil {
		cleanup()
		_ = handshakeR.Close()
		_ = lifelineW.Close()
		return nil, fmt.Errorf("vm lifeline handshake: %w", err)
	}

	// No context: the child's lifetime is the VM's (minutes to hours), owned
	// by krunVM.Stop via signals — a ctx cancellation killing the VMM behind
	// the lifecycle's back is exactly what must not happen.
	cmd := exec.Command(exePath, ChildCommand) //nolint:gosec,noctx // exePath is the daemon's own resolved binary; lifetime owned by Stop, not a ctx
	// A finite, parent-closed stream: the guest inherits an EOF'd pipe as its
	// stdin, never a live daemon descriptor.
	cmd.Stdin = &specJSON
	cmd.Stdout = console
	cmd.Stderr = console
	const lifelineExtra = 1
	cmd.ExtraFiles = []*os.File{handshakeW, lifelineR} // fd 3 and fd 4 in the child
	// The child gets a MINIMAL environment, built from scratch rather than
	// inherited: the daemon's env must not leak toward the guest, and that is
	// also what guarantees no OTHER process the daemon spawns can pick up a
	// lifeline variable — the daemon never carries one in its own environment.
	// (The guest's own env comes from the spec, independent of this.)
	cmd.Env = append(childEnv(os.Getenv, libkrunfwDirs),
		envLifelineFD+"="+strconv.Itoa(extraFileFD(lifelineExtra)),
		envLifelineNonce+"="+token)

	return &childCmd{cmd: cmd, handshake: handshakeR, lifeline: lifelineW, cleanup: cleanup}, nil
}

// childEnv builds the child's minimal environment.
// libLoaderPathVars are the dynamic-loader search paths forwarded from the
// daemon to the VM child.
//
// libkrun dlopens libkrunfw BY LEAF NAME (ADR-010 packaging finding), so the
// child must inherit whatever search path let the daemon itself link. Both
// platforms' variables are listed rather than one per build tag, because the
// forwarding rule is identical and a per-OS split is how one of them gets
// forgotten — which is exactly what happened: only the macOS variable was
// forwarded, and on Linux the child died with "Couldn't find or load
// libkrunfw.so.5". Only variables actually set are passed on.
var libLoaderPathVars = []string{
	"DYLD_FALLBACK_LIBRARY_PATH", // macOS
	"LD_LIBRARY_PATH",            // Linux
}

// childEnv builds the child's minimal environment. lookup and findLibDirs are
// injected so the rule is testable without mutating the process environment or
// depending on what this machine happens to have installed.
//
// Forwarding alone was not enough. Nobody exports a loader path on a normal
// install, and Homebrew's /opt/homebrew/lib is not on macOS's default fallback
// search path — so every VM child on a stock Mac died with "Couldn't find or
// load libkrunfw.5.dylib". Where the variable is unset, the directories that
// actually hold libkrunfw are appended; where it is set, the operator's value
// comes first, so a locally built libkrunfw is never silently overridden by a
// system copy. Refs: MGIT-61.15, ADR-010
func childEnv(lookup func(string) string, findLibDirs func() []string) []string {
	env := []string{"PATH=/usr/bin:/bin"}
	for _, key := range libLoaderPathVars {
		value := lookup(key)
		if key == loaderPathVar() {
			value = strings.Join(append(splitNonEmpty(value), findLibDirs()...), ":")
		}
		if value != "" {
			env = append(env, key+"="+value)
		}
	}
	return env
}

// splitNonEmpty splits a colon-separated search path, dropping empty entries.
func splitNonEmpty(v string) []string {
	if v == "" {
		return nil
	}
	return strings.Split(v, ":")
}

// loaderPathVar is the dynamic loader's search-path variable for this
// platform.
func loaderPathVar() string {
	if runtime.GOOS == "darwin" {
		return "DYLD_FALLBACK_LIBRARY_PATH"
	}
	return "LD_LIBRARY_PATH"
}

// libkrunfwSearchDirs are the install prefixes checked for libkrunfw, in
// preference order. They are the package managers' defaults: Homebrew on both
// architectures, and the usual system prefixes on Linux where libkrun is
// almost always built from source (no current Ubuntu release packages it).
// /usr/local/lib64 is listed because it is where a from-source Linux install
// actually lands — libkrunfw's Makefile puts a 64-bit build in $(PREFIX)/lib64
// — and Ubuntu's ld.so.conf does not cover it, so nothing else would.
var libkrunfwSearchDirs = []string{
	"/opt/homebrew/lib",
	"/usr/local/lib64",
	"/usr/local/lib",
	"/usr/lib",
	"/usr/lib64",
	"/usr/lib/" + runtime.GOARCH + "-linux-gnu",
}

// libkrunfwDirs returns the standard install directories that actually
// contain libkrunfw on this machine.
func libkrunfwDirs() []string { return libkrunfwDirsIn(libkrunfwSearchDirs) }

// libkrunfwDirsIn returns those of dirs holding a libkrunfw shared library.
// Directories that do not have it are dropped rather than padding the
// loader's search path with places it would only waste time in.
func libkrunfwDirsIn(dirs []string) []string {
	pattern := "libkrunfw.so*"
	if runtime.GOOS == "darwin" {
		pattern = "libkrunfw*.dylib"
	}
	var found []string
	for _, dir := range dirs {
		if hits, err := filepath.Glob(filepath.Join(dir, pattern)); err == nil && len(hits) > 0 {
			found = append(found, dir)
		}
	}
	return found
}

// execChild adapts a real *exec.Cmd to childProcess.
//
// It holds the parent's end of the lifeline for the child's whole life. That
// is not bookkeeping: os.File closes its descriptor from a finalizer, so an
// unreachable lifeline would be closed by the garbage collector and tell a
// live daemon's child that its daemon had died. Reachability comes from
// krunVM.child, which the manager holds for as long as the VM exists.
// Refs: MGIT-103
type execChild struct {
	cmd       *exec.Cmd
	handshake *os.File
	lifeline  *os.File
}

func (c *execChild) Handshake() io.Reader { return c.handshake }

func (c *execChild) Signal(sig os.Signal) error { return c.cmd.Process.Signal(sig) }

func (c *execChild) Kill() error { return c.cmd.Process.Kill() }

// Wait reaps the child, closes the parent's handshake and lifeline ends, and
// returns the exit code (-1 when killed by signal, per os.ProcessState). The
// lifeline is released only AFTER the reap: closing it earlier would be a
// second, redundant kill signal to a child that is already ending.
func (c *execChild) Wait() (int, error) {
	err := c.cmd.Wait()
	_ = c.handshake.Close()
	if c.lifeline != nil {
		_ = c.lifeline.Close()
	}
	if c.cmd.ProcessState == nil {
		return -1, err
	}
	return c.cmd.ProcessState.ExitCode(), err
}
