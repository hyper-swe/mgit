package main

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// The value of a named variable reaches the guest without ever being a
// command-line argument: the caller names it, mgit reads it from its own
// environment. Every value below is a made-up marker, never a real one.
const envNameMarker = "zz-marker-7f3a91c2"

// TestRun_EnvName_DeliversNamedVariableFromOwnEnvironment verifies `mgit run
// --env-name NAME` sends NAME=<value read from mgit's own environment> to the
// guest, only that one variable, and that the value never appears in output.
func TestRun_EnvName_DeliversNamedVariableFromOwnEnvironment(t *testing.T) {
	wt := filepath.FromSlash("/repo/wt")
	fc := &fakeSandboxClient{listResult: boundSandbox(wt, "MGIT-7.1"), execStdout: "ok\n"}
	t.Setenv("MGIT_TEST_NAMED", envNameMarker)
	t.Setenv("MGIT_TEST_OTHER", "not-selected")

	out, err := runRun(okConnect(fc), staticwd(wt), "--env-name", "MGIT_TEST_NAMED", "--", "true")

	require.NoError(t, err)
	assert.Equal(t, []string{"MGIT_TEST_NAMED=" + envNameMarker}, fc.execReq.Env,
		"exactly the named variable, no other host variable")
	assert.NotContains(t, out, envNameMarker)
}

// TestRun_EnvName_RemovesValueFromProcessEnvironment verifies that once the
// value is taken, mgit's own environment no longer holds it, so a helper
// process mgit starts afterward (the daemon) cannot inherit it.
func TestRun_EnvName_RemovesValueFromProcessEnvironment(t *testing.T) {
	wt := filepath.FromSlash("/repo/wt")
	var seenAtConnect string
	fc := &fakeSandboxClient{listResult: boundSandbox(wt, "MGIT-7.1")}
	connect := func(context.Context) (sandboxClient, error) {
		seenAtConnect = os.Getenv("MGIT_TEST_NAMED")
		return fc, nil
	}
	t.Setenv("MGIT_TEST_NAMED", envNameMarker)

	_, err := runRun(connect, staticwd(wt), "--env-name", "MGIT_TEST_NAMED", "--", "true")

	require.NoError(t, err)
	assert.Empty(t, seenAtConnect, "the value is gone before any helper can be started")
	assert.Equal(t, "MGIT_TEST_NAMED="+envNameMarker, fc.execReq.Env[0], "but it was delivered")
}

// TestRun_EnvName_RefusesBadInputBeforeConnecting verifies each unusable
// request is refused before any connection, and the refusal names the
// variable but never shows a value.
func TestRun_EnvName_RefusesBadInputBeforeConnecting(t *testing.T) {
	t.Setenv("MGIT_TEST_NAMED", envNameMarker)
	t.Setenv("MGIT_TEST_EMPTY", "")
	t.Setenv("MGIT_TEST_HUGE", strings.Repeat("x", 40<<10))
	cases := []struct {
		name string
		args []string
		want string
	}{
		{"missing", []string{"--env-name", "MGIT_TEST_ABSENT"}, "MGIT_TEST_ABSENT"},
		{"empty", []string{"--env-name", "MGIT_TEST_EMPTY"}, "MGIT_TEST_EMPTY"},
		{"oversize", []string{"--env-name", "MGIT_TEST_HUGE"}, "MGIT_TEST_HUGE"},
		{"bad grammar", []string{"--env-name", "MGIT=TEST"}, "MGIT=TEST"},
		{"leading digit", []string{"--env-name", "1BAD"}, "1BAD"},
		{"duplicate name", []string{"--env-name", "MGIT_TEST_NAMED", "--env-name", "MGIT_TEST_NAMED"}, "MGIT_TEST_NAMED"},
		{"also given as --env", []string{"--env-name", "MGIT_TEST_NAMED", "--env", "MGIT_TEST_NAMED=x"}, "MGIT_TEST_NAMED"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			connected := false
			connect := func(context.Context) (sandboxClient, error) {
				connected = true
				return &fakeSandboxClient{}, nil
			}
			args := append(append([]string{}, tc.args...), "--", "true")

			out, err := runRun(connect, staticwd(filepath.FromSlash("/repo/wt")), args...)

			require.Error(t, err)
			assert.False(t, connected, "refused before any connection")
			assert.Contains(t, out, tc.want)
			assert.NotContains(t, out, envNameMarker)
			assert.NotContains(t, out, strings.Repeat("x", 100))
		})
	}
}

// TestRun_Capabilities_ReportsEnvNameWithoutSideEffects verifies the
// capability report names the feature and runs before any lookup.
func TestRun_Capabilities_ReportsEnvNameWithoutSideEffects(t *testing.T) {
	connected := false
	connect := func(context.Context) (sandboxClient, error) {
		connected = true
		return nil, assert.AnError
	}

	out, err := runRun(connect, func() (string, error) { return "", assert.AnError }, "--capabilities=json")

	require.NoError(t, err)
	assert.False(t, connected)
	assert.JSONEq(t, `{"capabilities":["exec-env-name/v1"]}`, strings.TrimSpace(out))
}

// TestRun_Capabilities_RefusesAnyOtherFormat verifies an unknown format is an
// error, so a caller reading it as unsupported refuses to proceed.
func TestRun_Capabilities_RefusesAnyOtherFormat(t *testing.T) {
	out, err := runRun(errConnect(assert.AnError), staticwd("/x"), "--capabilities=yaml")
	require.Error(t, err)
	assert.Contains(t, out, "use json")
}

// TestSandboxExec_EnvName_DeliversNamedVariable verifies the same flag on
// `mgit sandbox exec`.
func TestSandboxExec_EnvName_DeliversNamedVariable(t *testing.T) {
	fc := &fakeSandboxClient{}
	t.Setenv("MGIT_TEST_NAMED", envNameMarker)

	out, err := runSandbox(okConnect(fc), "exec", "--task-id", "MGIT-7.1",
		"--env-name", "MGIT_TEST_NAMED", "--", "true")

	require.NoError(t, err)
	assert.Equal(t, []string{"MGIT_TEST_NAMED=" + envNameMarker}, fc.execReq.Env)
	assert.NotContains(t, out, envNameMarker)
	_, still := os.LookupEnv("MGIT_TEST_NAMED")
	assert.False(t, still, "removed from the process environment after use")
}

// TestSandboxExec_EnvName_RefusesMissingBeforeConnecting mirrors the run
// refusal for the exec verb.
func TestSandboxExec_EnvName_RefusesMissingBeforeConnecting(t *testing.T) {
	connected := false
	connect := func(context.Context) (sandboxClient, error) {
		connected = true
		return &fakeSandboxClient{}, nil
	}

	out, err := runSandbox(connect, "exec", "--task-id", "MGIT-7.1",
		"--env-name", "MGIT_TEST_ABSENT", "--", "true")

	require.Error(t, err)
	assert.False(t, connected)
	assert.Contains(t, out, "MGIT_TEST_ABSENT")
}
