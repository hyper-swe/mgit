package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"time"

	"github.com/hyper-swe/mgit/internal/doctor"
)

// daemonVMMProbeTimeout bounds `mgit-sandboxd --vmm`. On a libkrun build the
// daemon re-execs a probe child (bounded at 10 s itself) that makes one VM
// context; a probe that outlasts this is its own finding.
const daemonVMMProbeTimeout = 20 * time.Second

// probeDaemonVMM locates this install's sandbox daemon and asks it which VMM
// it links and whether it can boot a guest. Refs: MGIT-229
func probeDaemonVMM(ctx context.Context) (doctor.DaemonVMM, error) {
	path, err := locateSandboxd()
	if err != nil {
		return doctor.DaemonVMM{}, err
	}
	return probeDaemonVMMAt(ctx, path)
}

// probeDaemonVMMAt runs the daemon at path with --vmm and reads its report.
// Anything but a report is an error naming what the daemon said instead: an
// older build rejects the flag, and a daemon the loader refuses never gets to
// answer (daemon/loads names that failure). Refs: MGIT-229
func probeDaemonVMMAt(ctx context.Context, path string) (doctor.DaemonVMM, error) {
	if _, err := os.Stat(path); err != nil {
		return doctor.DaemonVMM{}, fmt.Errorf("sandbox daemon binary: %w", err)
	}
	ctx, cancel := context.WithTimeout(ctx, daemonVMMProbeTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, path, "--vmm") //nolint:gosec // the daemon this install resolved
	cmd.WaitDelay = time.Second
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	runErr := cmd.Run()
	said := strings.TrimSpace(stderr.String())
	if strings.Contains(said, "flag provided but not defined: -vmm") {
		return doctor.DaemonVMM{}, fmt.Errorf("%s predates --vmm, so it cannot say whether it can boot a guest; "+
			"install mgit and mgit-sandboxd from one release", path)
	}
	if runErr != nil {
		if errors.Is(ctx.Err(), context.DeadlineExceeded) {
			return doctor.DaemonVMM{}, fmt.Errorf("%s --vmm gave no answer within %s", path, daemonVMMProbeTimeout)
		}
		return doctor.DaemonVMM{}, fmt.Errorf("%s --vmm failed (%w); it said: %s", path, runErr, firstNonEmptyLine(said))
	}
	v := doctor.DaemonVMM{Path: path}
	if err := json.Unmarshal(bytes.TrimSpace(stdout.Bytes()), &v.Report); err != nil || v.Report.VMM == "" {
		return doctor.DaemonVMM{}, fmt.Errorf("%s --vmm printed something that is not a VMM report: %q",
			path, firstNonEmptyLine(stdout.String()))
	}
	return v, nil
}
