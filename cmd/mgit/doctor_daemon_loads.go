package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"runtime"
	"strings"
	"syscall"
	"time"

	"github.com/hyper-swe/mgit/internal/doctor"
)

// daemonLoadProbeTimeout bounds `mgit-sandboxd --version`. The flag is
// answered before the daemon touches the host (no socket, no lock), so a
// healthy binary answers in milliseconds; the bound exists for the one that
// cannot. Refs: MGIT-206
const daemonLoadProbeTimeout = 10 * time.Second

// probeDaemonLoads locates this install's sandbox daemon and runs it with
// --version, for the doctor row daemon/loads. Refs: MGIT-206
func probeDaemonLoads(ctx context.Context) (doctor.DaemonLoad, error) {
	path, err := locateSandboxd()
	if err != nil {
		return doctor.DaemonLoad{}, err
	}
	return probeDaemonLoadsAt(ctx, path)
}

// probeDaemonLoadsAt runs the daemon at path with --version and reads what
// happened: a version line when it loaded, or its last words — parsed with
// the same loader-line recognizer the activation path uses, so the row and
// the verbs name the same missing library. A binary that is not there is an
// error (the caller reports not-checked); one that is there is always a
// verdict. Refs: MGIT-206, MGIT-61.15
func probeDaemonLoadsAt(ctx context.Context, path string) (doctor.DaemonLoad, error) {
	if _, err := os.Stat(path); err != nil {
		return doctor.DaemonLoad{}, fmt.Errorf("sandbox daemon binary: %w", err)
	}
	ctx, cancel := context.WithTimeout(ctx, daemonLoadProbeTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, path, "--version") //nolint:gosec // the daemon this install resolved
	// A killed daemon may leave a child holding the pipe (a hung one under
	// a shell did, in the probe's own test); do not wait on that child.
	cmd.WaitDelay = time.Second
	out, runErr := cmd.CombinedOutput()
	load := doctor.DaemonLoad{Path: path}
	text := strings.TrimSpace(string(out))
	if runErr == nil {
		load.Version = firstNonEmptyLine(text)
		if load.Version == "" {
			load.Version = "an empty version line"
		}
		return load, nil
	}
	load.Output = text
	switch {
	case errors.Is(ctx.Err(), context.DeadlineExceeded):
		load.Output = strings.TrimSpace(fmt.Sprintf("%s\n(no answer within %s)", text, daemonLoadProbeTimeout))
	case killedBySIGKILL(runErr):
		// No words to read: the kernel refused to run it. Refs: MGIT-212
		load.Output = strings.TrimSpace(fmt.Sprintf("%s\nkilled before it could run (SIGKILL: %s)", text, runErr.Error()))
		load.Remedy = sigkillRemedy(path)
	case load.Output == "":
		load.Output = runErr.Error()
	}
	if lib, remedy := loaderRemedy(text, path); remedy != "" {
		load.MissingLibrary, load.Remedy = lib, remedy
	}
	return load, nil
}

// killedBySIGKILL reports whether the probe's process died by SIGKILL — the
// shape of a code-signature refusal on darwin, which prints nothing.
func killedBySIGKILL(err error) bool {
	var ee *exec.ExitError
	if !errors.As(err, &ee) || ee.ProcessState == nil {
		return false
	}
	ws, ok := ee.Sys().(interface {
		Signaled() bool
		Signal() syscall.Signal
	})
	return ok && ws.Signaled() && ws.Signal() == syscall.SIGKILL
}

// sigkillRemedy names the two known reasons a daemon binary is SIGKILLed on
// exec on darwin and the fix for each, plus where the kernel wrote which one
// it was — the process itself could say nothing (MGIT-214: eight refusals in
// the unified log while daemon.log stayed empty). On other platforms the
// probe knows no cause and invents none. Refs: MGIT-212, MGIT-64
func sigkillRemedy(path string) string {
	if runtime.GOOS != "darwin" {
		return ""
	}
	return fmt.Sprintf("The kernel refused to run %s (a code-signature refusal is reported as `signal: killed`, with no words). "+
		"Two known causes, with different fixes: (1) a quarantined download — `xattr -d com.apple.quarantine %s` (MGIT-64); "+
		"(2) the file was overwritten in place (`cp` over the installed binary rewrites the inode under a cached signature) — "+
		"remove it and copy the new one in (`rm %s && cp <new> %s`, or `install -m 0755 <new> %s`); do not overwrite it (MGIT-212). "+
		"The kernel's own record says which: `log show --predicate 'process == \"kernel\"' --last 30m | grep -E 'AMFI|ASP|code signature'`.",
		path, path, path, path, path)
}

// firstNonEmptyLine is the first line with content, or "".
func firstNonEmptyLine(s string) string {
	for _, line := range strings.Split(s, "\n") {
		if t := strings.TrimSpace(line); t != "" {
			return t
		}
	}
	return ""
}
