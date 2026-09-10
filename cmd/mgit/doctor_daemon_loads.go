package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strings"
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
	if errors.Is(ctx.Err(), context.DeadlineExceeded) {
		load.Output = strings.TrimSpace(fmt.Sprintf("%s\n(no answer within %s)", text, daemonLoadProbeTimeout))
	} else if load.Output == "" {
		load.Output = runErr.Error()
	}
	if lib := missingLibrary(text); lib != "" {
		load.MissingLibrary = lib
		load.Remedy = missingLibraryRemedy(lib)
	}
	return load, nil
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
