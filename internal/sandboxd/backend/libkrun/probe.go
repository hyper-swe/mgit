package libkrun

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
	"time"

	"github.com/hyper-swe/mgit/internal/model"
)

// ProbeCommand is the hidden re-exec subcommand that answers
// `mgit-sandboxd --vmm` for a libkrun build: the child makes (and frees) one
// libkrun context, which is what makes libkrun load libkrunfw, and prints
// what the dynamic loader mapped. Refs: MGIT-229
const ProbeCommand = "__krun-probe"

// probeTimeout bounds the probe child. Making a context loads two libraries
// and starts nothing; a child that takes longer is itself the finding.
const probeTimeout = 10 * time.Second

// probeStderrTail bounds how much of a failed child's stderr a report keeps.
// The loader writes its reason last, so the END is kept.
const probeStderrTail = 1200

// Describe reports what this daemon binary's libkrun can do, asked the way a
// VM boot asks it: in a re-exec'd child of exePath carrying exactly the VM
// child's environment (childEnv), so the loader search that decides whether
// a guest can boot is the one measured. Refs: MGIT-229, MGIT-61.15
func Describe(ctx context.Context, exePath string) model.VMMReport {
	ctx, cancel := context.WithTimeout(ctx, probeTimeout)
	defer cancel()
	return runProbe(probeCmd(ctx, exePath))
}

// probeCmd builds the probe child's command: the daemon binary, the probe
// subcommand, and the VM child's environment.
func probeCmd(ctx context.Context, exePath string) *exec.Cmd {
	cmd := exec.CommandContext(ctx, exePath, ProbeCommand) //nolint:gosec // exePath is the daemon's own resolved binary
	cmd.Env = childEnv(os.Getenv, libkrunfwDirs)
	cmd.WaitDelay = time.Second
	return cmd
}

// runProbe runs a built probe command and reads its report.
func runProbe(cmd *exec.Cmd) model.VMMReport {
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	err := cmd.Run()
	return describeFromProbe(stdout.Bytes(), stderr.Bytes(), err)
}

// describeFromProbe turns the probe child's output into a report: the
// child's own report when it printed one, otherwise a problem that carries
// what the child said on the way down — a loader that refused the binary
// writes its reason there, and nowhere else. Refs: MGIT-229
func describeFromProbe(stdout, stderr []byte, runErr error) model.VMMReport {
	var r model.VMMReport
	out := bytes.TrimSpace(stdout)
	if len(out) > 0 && json.Unmarshal(out, &r) == nil && r.VMM != "" {
		return r
	}
	failed := model.VMMReport{VMM: model.BackendLibkrun}
	said := tail(strings.TrimSpace(string(stderr)), probeStderrTail)
	switch {
	case runErr != nil:
		failed.Problems = []string{fmt.Sprintf("the libkrun probe child failed (%v); it said: %s", runErr, orNothing(said))}
	case len(out) == 0:
		failed.Problems = []string{"the libkrun probe child printed no report; it said: " + orNothing(said)}
	default:
		failed.Problems = []string{"the libkrun probe child's report could not be read: " +
			tail(string(out), probeStderrTail)}
	}
	return failed
}

// ProbeMain runs in the probe child: it asks the linked libkrun what it can
// load and prints the report as JSON. It returns the process exit code.
func ProbeMain(out io.Writer) int {
	if err := json.NewEncoder(out).Encode(probeInProcess()); err != nil {
		return 1
	}
	return 0
}

// tail keeps the last n bytes of s, cut at a line boundary.
func tail(s string, n int) string {
	if len(s) <= n {
		return s
	}
	s = s[len(s)-n:]
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		s = s[i+1:]
	}
	return "… " + s
}

// orNothing names an empty capture instead of printing a blank.
func orNothing(s string) string {
	if s == "" {
		return "(nothing)"
	}
	return s
}
