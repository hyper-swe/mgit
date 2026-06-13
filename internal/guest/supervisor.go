// Package guest implements the mgit-guest supervisor: the PID-1 agent
// baked into rootfs images that serves exec requests over vsock. It is
// PURE TRANSPORT for the trust boundary — it holds no signing key and
// cannot mint attestations (SEC-01); the host attests landed commits.
// Every child runs in a CLEAN environment: the host environment is
// never inherited, only an explicit base plus per-exec injections
// reach the child (FR-17.3). Whole-command routing means the host
// already wrapped the shell, so the guest just execs the argv it is
// given (FR-17.11). Refs: FR-17.11, FR-17.3, SEC-01, MGIT-11.5.6
package guest

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os/exec"
	"time"

	"github.com/hyper-swe/mgit/internal/model"
)

// ResourceUsage is the child's CPU usage, reported to the host.
type ResourceUsage struct {
	UserTime   time.Duration `json:"user_time_ns"`
	SystemTime time.Duration `json:"system_time_ns"`
}

// Outcome is one exec's result: exit code plus resource usage. Stdout
// and stderr are streamed during the run, not buffered here.
type Outcome struct {
	ExitCode int           `json:"exit_code"`
	Usage    ResourceUsage `json:"usage"`
}

// Supervisor runs guest commands. BaseEnv is the clean environment the
// child starts from; the host environment is deliberately absent.
type Supervisor struct {
	BaseEnv []string
	Logger  *slog.Logger
}

// NewSupervisor returns a supervisor with the default clean base env.
func NewSupervisor(logger *slog.Logger) *Supervisor {
	return &Supervisor{BaseEnv: defaultBaseEnv(), Logger: logger}
}

// defaultBaseEnv is the minimal clean environment every guest child
// starts from — never the host's environment (SEC-01, FR-17.3).
func defaultBaseEnv() []string {
	return []string{
		"PATH=/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin",
		"HOME=/work",
		"TMPDIR=/tmp",
	}
}

// Execute runs one whole command, streaming stdout/stderr to the given
// writers and returning the exit code and resource usage. The child's
// environment is BaseEnv + req.Env ONLY — the inherited host
// environment is never read, so no host variable can leak into the
// guest (SEC-01, FR-17.3). A non-zero exit is a normal outcome, not a
// supervisor error; only a failure to start the process is an error.
// Refs: FR-17.11
func (s *Supervisor) Execute(ctx context.Context, req model.ExecRequest, stdout, stderr io.Writer) (Outcome, error) {
	if err := req.Validate(); err != nil {
		return Outcome{}, fmt.Errorf("guest exec: %w", err)
	}

	cmd := exec.CommandContext(ctx, req.Command[0], req.Command[1:]...) //nolint:gosec // argv is the host-routed whole command (FR-17.11)
	// Clean environment: explicit base + per-exec injections, never the
	// inherited (host) environment.
	cmd.Env = make([]string, 0, len(s.BaseEnv)+len(req.Env))
	cmd.Env = append(cmd.Env, s.BaseEnv...)
	cmd.Env = append(cmd.Env, req.Env...)
	if req.Dir != "" {
		cmd.Dir = req.Dir
	}
	cmd.Stdout = stdout
	cmd.Stderr = stderr

	runErr := cmd.Run()

	var outcome Outcome
	if cmd.ProcessState != nil {
		outcome.ExitCode = cmd.ProcessState.ExitCode()
		outcome.Usage = ResourceUsage{
			UserTime:   cmd.ProcessState.UserTime(),
			SystemTime: cmd.ProcessState.SystemTime(),
		}
	}
	if runErr != nil {
		var exitErr *exec.ExitError
		if errors.As(runErr, &exitErr) {
			return outcome, nil // non-zero exit is a valid outcome
		}
		return outcome, fmt.Errorf("guest exec: start %q: %w", req.Command[0], runErr)
	}
	return outcome, nil
}
