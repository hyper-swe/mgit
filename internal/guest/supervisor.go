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
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/hyper-swe/mgit/internal/execwire"
	"github.com/hyper-swe/mgit/internal/model"
)

// ResourceUsage and Outcome are the exec wire result types, owned by the
// execwire package so the host client and the guest share one definition.
// They are aliased here for the supervisor's existing call sites.
type (
	// ResourceUsage is the child's CPU usage, reported to the host.
	ResourceUsage = execwire.ResourceUsage
	// Outcome is one exec's result: exit code plus resource usage. Stdout
	// and stderr are streamed during the run, not buffered here.
	Outcome = execwire.Result
)

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

	// Clean environment: explicit base + per-exec injections, never the
	// inherited (host) environment.
	env := make([]string, 0, len(s.BaseEnv)+len(req.Env))
	env = append(env, s.BaseEnv...)
	env = append(env, req.Env...)

	// Resolve the program against the GUEST's PATH (from env), not PID 1's
	// ambient environment. exec.Command would LookPath req.Command[0] against
	// the running process's own PATH — but mgit-guest runs as PID 1 with no
	// PATH set, so a bare command (`echo`, `npm`) would fail "executable file
	// not found" even though the child env carries a correct PATH. Resolving
	// here makes `mgit run -- <bare cmd>` behave like a shell. Refs: FR-17.11
	prog, err := lookPathIn(req.Command[0], envValue(env, "PATH"))
	if err != nil {
		return Outcome{}, fmt.Errorf("guest exec: %w", err)
	}

	cmd := exec.CommandContext(ctx, prog, req.Command[1:]...) //nolint:gosec // argv is the host-routed whole command (FR-17.11)
	cmd.Env = env
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

// envValue returns the value of the last key= entry in env (last-wins, the
// same precedence the OS applies), or "" when unset.
func envValue(env []string, key string) string {
	prefix := key + "="
	val := ""
	for _, e := range env {
		if strings.HasPrefix(e, prefix) {
			val = e[len(prefix):]
		}
	}
	return val
}

// lookPathIn resolves prog against pathEnv (a ":"-separated PATH), mirroring
// exec.LookPath but searching the GUEST's PATH rather than the running
// process's. A prog containing a separator is returned unchanged (run as
// given, relative to the child's Dir). Otherwise each PATH entry is probed
// for an executable regular file. Refs: FR-17.11
func lookPathIn(prog, pathEnv string) (string, error) {
	if strings.ContainsRune(prog, filepath.Separator) {
		return prog, nil
	}
	for _, dir := range filepath.SplitList(pathEnv) {
		if dir == "" {
			continue
		}
		cand := filepath.Join(dir, prog)
		if isExecutable(cand) {
			return cand, nil
		}
	}
	return "", fmt.Errorf("%q: executable file not found in $PATH", prog)
}

// isExecutable reports whether path is a regular file with an executable bit.
func isExecutable(path string) bool {
	fi, err := os.Stat(path)
	if err != nil || fi.IsDir() {
		return false
	}
	return fi.Mode().Perm()&0o111 != 0
}
