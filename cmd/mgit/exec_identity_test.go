package main

import (
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/hyper-swe/mgit/internal/model"
)

// A guest exec whose identity the daemon could not verify is said on
// stderr, in the daemon's words, after the command's own output; a verified
// one, or no verdict at all (an older daemon), is silent. The exit code is
// the command's either way. Refs: MGIT-151
func TestRun_IdentityVerdict_UnverifiedIsSaidVerifiedIsSilent(t *testing.T) {
	agent := model.GuestIdentity{UID: 501, GID: 20, Name: "agent", Home: "/home/agent"}
	unverified := model.VerdictOnExecIdentity(&agent, nil)
	verified := model.VerdictOnExecIdentity(&agent, &agent)
	tests := []struct {
		name     string
		identity *model.ExecIdentity
		wantSaid bool
	}{
		{name: "unverified_is_said", identity: &unverified, wantSaid: true},
		{name: "verified_is_silent", identity: &verified},
		{name: "no_verdict_from_an_older_daemon_is_silent", identity: nil},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			wt := filepath.FromSlash("/repo/wt")
			fc := &fakeSandboxClient{listResult: boundSandbox(wt, "MGIT-151"), execStdout: "hi\n", execIdentity: tt.identity}
			out, err := runRun(okConnect(fc), staticwd(wt), "--", "id")
			require.NoError(t, err)
			assert.Contains(t, out, "hi\n", "the command's output is relayed")
			if tt.wantSaid {
				assert.Contains(t, out, "guest exec identity UNVERIFIED")
				assert.Contains(t, out, "did not report")
				assert.Contains(t, out, "asked for uid 501 gid 20")
			} else {
				assert.Equal(t, "hi\n", out, "nothing but the command's output")
			}
		})
	}
}

// `--as-root` is the only escalation: it sets as_root on the request and
// nothing else; without it the request names no identity — the daemon
// chooses. Refs: MGIT-151
func TestRun_AsRoot_SetsTheEscalationOnTheRequest(t *testing.T) {
	for _, asRoot := range []bool{false, true} {
		wt := filepath.FromSlash("/repo/wt")
		fc := &fakeSandboxClient{listResult: boundSandbox(wt, "MGIT-151")}
		args := []string{"--", "apt-get", "install", "curl"}
		if asRoot {
			args = append([]string{"--as-root"}, args...)
		}
		_, err := runRun(okConnect(fc), staticwd(wt), args...)
		require.NoError(t, err)
		assert.Equal(t, asRoot, fc.execReq.AsRoot, "as_root follows the flag")
		assert.Nil(t, fc.execReq.RunAs, "the CLI never chooses an identity")
	}
}

// The same two properties on `sandbox exec`, with a mismatch verdict.
// Refs: MGIT-151
func TestSandboxExec_IdentityVerdictAndAsRoot(t *testing.T) {
	agent := model.GuestIdentity{UID: 501, GID: 20}
	root := model.RootIdentity()
	mismatch := model.VerdictOnExecIdentity(&agent, &root)
	fc := &fakeSandboxClient{execStdout: "ok\n", execIdentity: &mismatch}
	out, err := runSandbox(okConnect(fc), "exec", "--task-id", "MGIT-151", "--as-root", "--", "id")
	require.NoError(t, err)
	assert.True(t, fc.execReq.AsRoot)
	assert.Contains(t, out, "ok\n")
	assert.Contains(t, out, "guest exec identity UNVERIFIED")
	assert.Contains(t, out, "ran as uid 0 gid 0, not the uid 501 gid 20 asked for")
}

// The help says what commands run as now, and how to escalate; the old
// "runs as root until MGIT-151" sentence is retired with the ticket.
// Refs: MGIT-151
func TestSandboxExec_Help_SaysWhatCommandsRunAsAndHowToEscalate(t *testing.T) {
	out, err := runSandbox(okConnect(&fakeSandboxClient{}), "exec", "--help")
	require.NoError(t, err)
	assert.Contains(t, out, "--as-root")
	assert.Contains(t, out, "daemon's own user")
	assert.NotContains(t, out, "Until MGIT-151 lands")
}
