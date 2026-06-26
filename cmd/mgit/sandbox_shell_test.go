package main

import (
	"bytes"
	"errors"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/hyper-swe/mgit/internal/model"
)

// runShell drives `mgit sandbox shell` with the given args, connector, and
// stdin, capturing combined stdout+stderr.
func runShell(connect connectFunc, stdin string, args ...string) (string, error) {
	cmd := newSandboxCmd(connect)
	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(&out)
	cmd.SetIn(strings.NewReader(stdin))
	cmd.SetArgs(append([]string{"shell"}, args...))
	err := cmd.Execute()
	return out.String(), err
}

// TestT2_ShellAttach verifies `mgit sandbox shell` attaches to the task's
// sandbox, proxies stdin into the session and the session output back, and
// propagates the exit code. Refs: MGIT-11.11.4
func TestT2_ShellAttach(t *testing.T) {
	fc := &fakeSandboxClient{execStdout: "agent> ready\n"}
	out, err := runShell(okConnect(fc), "help\n", "--task", "MGIT-4.2")

	require.NoError(t, err)
	assert.Equal(t, "MGIT-4.2", fc.shellTask, "attached to the task's sandbox")
	assert.Equal(t, "help\n", fc.shellStdin, "stdin proxied into the session")
	assert.Contains(t, out, "agent> ready", "session output proxied back")
}

// TestT2_Shell_RequiresTask verifies the task flag is required. Refs: MGIT-11.11.4
func TestT2_Shell_RequiresTask(t *testing.T) {
	out, err := runShell(okConnect(&fakeSandboxClient{}), "")
	require.Error(t, err)
	assert.Contains(t, out, "--task-id is required")
}

// TestT2_Shell_PropagatesExit verifies a non-zero session exit becomes an
// exitError. Refs: MGIT-11.11.4
func TestT2_Shell_PropagatesExit(t *testing.T) {
	fc := &fakeSandboxClient{execCode: 3}
	_, err := runShell(okConnect(fc), "", "--task", "MGIT-4.2")
	var ee *exitError
	require.True(t, errors.As(err, &ee))
	assert.Equal(t, 3, ee.code)
}

// TestT2_Shell_DaemonUnavailable_FailsClosed verifies an unreachable daemon
// is a clear error, never a local shell. Refs: MGIT-11.11.4, NFR-17.6
func TestT2_Shell_DaemonUnavailable_FailsClosed(t *testing.T) {
	_, err := runShell(errConnect(errors.New("daemon down")), "", "--task", "MGIT-4.2")
	require.Error(t, err)
	assert.NotContains(t, err.Error(), "exit status", "no session, so no exit code")
}

// TestT2_Shell_TransportError_Surfaced verifies a transport error (e.g. the
// KVM-gated guest PTY transport being unavailable) is surfaced clearly.
// Refs: MGIT-11.11.4
func TestT2_Shell_TransportError_Surfaced(t *testing.T) {
	fc := &fakeSandboxClient{execErr: model.ErrShellTransportUnavailable}
	out, err := runShell(okConnect(fc), "", "--task", "MGIT-4.2")
	require.Error(t, err)
	assert.Contains(t, out, "shell")
}
