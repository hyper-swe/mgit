package main

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/hyper-swe/mgit/internal/controlproto"
)

// TestSandboxPolicySet_ReplacesTheAllowlist verifies `policy set` forwards the
// replacement entries and reports what is now in force. Refs: MGIT-72
func TestSandboxPolicySet_ReplacesTheAllowlist(t *testing.T) {
	fc := &fakeSandboxClient{policyResult: &controlproto.PolicyResult{
		Entries: []string{"registry.npmjs.org:443"}, RuleCount: 1,
	}}

	out, err := runSandbox(okConnect(fc), "policy", "set", "--task", "MGIT-72",
		"--allow", "registry.npmjs.org:443")

	require.NoError(t, err)
	assert.Equal(t, "MGIT-72", fc.policyTID)
	assert.Equal(t, []string{"registry.npmjs.org:443"}, fc.policyEntries)
	assert.False(t, fc.policyDrain)
	assert.Contains(t, out, "registry.npmjs.org:443")
}

// TestSandboxPolicySet_RepeatedAllowFlagsAccumulate verifies several
// destinations can be granted in ONE atomic replacement — applying them one at
// a time would make intermediate policies observable. Refs: MGIT-72
func TestSandboxPolicySet_RepeatedAllowFlagsAccumulate(t *testing.T) {
	fc := &fakeSandboxClient{policyResult: &controlproto.PolicyResult{RuleCount: 2}}

	_, err := runSandbox(okConnect(fc), "policy", "set", "--task", "MGIT-72",
		"--allow", "a.example:443", "--allow", "b.example:80")

	require.NoError(t, err)
	assert.Equal(t, []string{"a.example:443", "b.example:80"}, fc.policyEntries)
}

// TestSandboxPolicyRevoke_KillsEstablishedFlowsByDefault is the default that
// carries the guarantee: an unqualified revoke sends NO entries and does NOT
// drain, and the output says how many established flows it terminated.
// Refs: MGIT-72, ADR-012
func TestSandboxPolicyRevoke_KillsEstablishedFlowsByDefault(t *testing.T) {
	fc := &fakeSandboxClient{policyResult: &controlproto.PolicyResult{Killed: 2}}

	out, err := runSandbox(okConnect(fc), "policy", "revoke", "--task", "MGIT-72")

	require.NoError(t, err)
	assert.Empty(t, fc.policyEntries, "a revoke replaces the policy with nothing")
	assert.False(t, fc.policyDrain, "kill is the default; drain must be asked for by name")
	assert.Contains(t, out, "2", "the operator is told what was terminated")
	assert.Contains(t, strings.ToLower(out), "terminated")
}

// TestSandboxPolicyRevoke_DrainIsOptIn is the matching positive control: the
// weaker behavior is reachable, only by name, and is reported as such so a
// caller can never be unsure which one they got. Refs: MGIT-72, ADR-012
func TestSandboxPolicyRevoke_DrainIsOptIn(t *testing.T) {
	fc := &fakeSandboxClient{policyResult: &controlproto.PolicyResult{Drained: true}}

	out, err := runSandbox(okConnect(fc), "policy", "revoke", "--task", "MGIT-72", "--drain")

	require.NoError(t, err)
	assert.True(t, fc.policyDrain)
	assert.Contains(t, strings.ToLower(out), "drain")
}

// TestSandboxPolicyShow_ReportsTheLivePolicy verifies the read verb prints the
// policy in force, which after a mutation is not the launch-time one.
// Refs: MGIT-72
func TestSandboxPolicyShow_ReportsTheLivePolicy(t *testing.T) {
	fc := &fakeSandboxClient{policyResult: &controlproto.PolicyResult{
		Entries: []string{"a.example:443"}, RuleCount: 1,
	}}

	out, err := runSandbox(okConnect(fc), "policy", "show", "--task", "MGIT-72")

	require.NoError(t, err)
	assert.Equal(t, "MGIT-72", fc.policyShowTID)
	assert.Contains(t, out, "a.example:443")
}

// TestSandboxPolicyShow_EmptyIsStatedPlainly verifies a fully revoked policy
// reads as "no egress", not as blank output a caller could mistake for a
// missing answer. Refs: MGIT-72
func TestSandboxPolicyShow_EmptyIsStatedPlainly(t *testing.T) {
	fc := &fakeSandboxClient{policyResult: &controlproto.PolicyResult{}}

	out, err := runSandbox(okConnect(fc), "policy", "show", "--task", "MGIT-72")

	require.NoError(t, err)
	assert.Contains(t, strings.ToLower(out), "no egress")
}

// TestSandboxPolicy_JSON emits machine-readable output for an agent caller.
func TestSandboxPolicy_JSON(t *testing.T) {
	fc := &fakeSandboxClient{policyResult: &controlproto.PolicyResult{
		Entries: []string{"a.example:443"}, RuleCount: 1, Killed: 3,
	}}

	out, err := runSandbox(okConnect(fc), "policy", "revoke", "--task", "MGIT-72", "--json")
	require.NoError(t, err)

	var got controlproto.PolicyResult
	require.NoError(t, json.Unmarshal([]byte(out), &got))
	assert.Equal(t, 3, got.Killed)
}

// TestSandboxPolicy_MissingTask rejects every verb without --task, so a
// mutation can never land on a sandbox the caller did not name.
func TestSandboxPolicy_MissingTask(t *testing.T) {
	for _, verb := range []string{"set", "revoke", "show"} {
		t.Run(verb, func(t *testing.T) {
			_, err := runSandbox(okConnect(&fakeSandboxClient{}), "policy", verb)
			require.Error(t, err)
			assert.Contains(t, err.Error(), "--task")
		})
	}
}

// TestSandboxPolicySet_WithoutAllow_IsRefused verifies `set` with no
// destinations is REFUSED rather than quietly performing a full revoke.
//
// The two are the same operation underneath, and that is exactly why the CLI
// must not let one be typed by accident: a caller who meant to grant and
// mistyped the flag would silently revoke everything. `revoke` says so by
// name. Refs: MGIT-72
func TestSandboxPolicySet_WithoutAllow_IsRefused(t *testing.T) {
	fc := &fakeSandboxClient{}

	_, err := runSandbox(okConnect(fc), "policy", "set", "--task", "MGIT-72")

	require.Error(t, err)
	assert.Contains(t, err.Error(), "revoke")
	assert.Empty(t, fc.policyTID, "nothing may be applied when the intent is ambiguous")
}

// TestSandboxPolicy_DaemonError_IsSurfaced verifies a failure to reach the
// enforcer is reported, never swallowed into a cheerful success — a caller who
// believes egress is closed when it is open is the hazard. Refs: MGIT-72
func TestSandboxPolicy_DaemonError_IsSurfaced(t *testing.T) {
	fc := &fakeSandboxClient{opErr: errors.New("vm control channel unreachable")}

	_, err := runSandbox(okConnect(fc), "policy", "revoke", "--task", "MGIT-72")

	require.Error(t, err)
	assert.Contains(t, err.Error(), "unreachable")
}

// TestSandboxCmd_Help_ListsPolicy verifies the verb is discoverable from the
// command group — an agent finds it by reading `mgit sandbox --help`.
func TestSandboxCmd_Help_ListsPolicy(t *testing.T) {
	out, err := runSandbox(okConnect(&fakeSandboxClient{}), "--help")
	require.NoError(t, err)
	assert.Contains(t, out, "policy")
}

// TestSandboxPolicy_HelpDocumentsTheEstablishedFlowChoice verifies the
// established-flow decision is documented AT THE VERB.
//
// This is not decoration: kill and drain are opposite security postures, and a
// caller who assumes the other one is exposed. The help must say which one
// they get by default and why. Refs: MGIT-72, ADR-012
func TestSandboxPolicy_HelpDocumentsTheEstablishedFlowChoice(t *testing.T) {
	out, err := runSandbox(okConnect(&fakeSandboxClient{}), "policy", "revoke", "--help")
	require.NoError(t, err)

	low := strings.ToLower(out)
	assert.Contains(t, low, "established", "the help must address in-flight connections")
	assert.Contains(t, low, "--drain", "the opt-in must be named")
	assert.Contains(t, low, "terminat", "the default must be stated as terminating them")
}
