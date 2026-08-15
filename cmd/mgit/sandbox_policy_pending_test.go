package main

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/hyper-swe/mgit/internal/controlproto"
	"github.com/hyper-swe/mgit/internal/model"
)

// TestSandboxPolicyShow_PendingPolicy_IsNotPresentedAsInForce is the operator's
// half of MGIT-109. `policy show` on a registered-but-unbooted sandbox reports
// what WILL be enforced; describing it as "in force" would tell a caller a line
// is being held that nothing is holding yet. Refs: MGIT-109, FR-17.10, SEC-04
func TestSandboxPolicyShow_PendingPolicy_IsNotPresentedAsInForce(t *testing.T) {
	fc := &fakeSandboxClient{policyResult: &controlproto.PolicyResult{
		Entries: []string{"example.com:443"}, Pending: true,
	}}

	out, err := runSandbox(okConnect(fc), "policy", "show", "--task", "MGIT-109")

	require.NoError(t, err)
	assert.Contains(t, out, "PENDING")
	assert.Contains(t, out, "example.com:443")
	assert.Contains(t, out, "has not booted")
	assert.NotContains(t, out, "in force",
		"a staged policy must never be described as one being enforced")
	assert.NotContains(t, out, "terminated",
		"a VM that has never run has no established flows; a count would reassure falsely")
}

// TestSandboxPolicySet_PendingPolicy_JSONCarriesThePendingFlag verifies the
// distinction survives into machine-readable output, where a consumer decides
// whether containment is actually up. Refs: MGIT-109
func TestSandboxPolicySet_PendingPolicy_JSONCarriesThePendingFlag(t *testing.T) {
	fc := &fakeSandboxClient{policyResult: &controlproto.PolicyResult{
		Entries: []string{"other.example.com:443"}, Pending: true,
	}}

	out, err := runSandbox(okConnect(fc), "policy", "set", "--task", "MGIT-109",
		"--allow", "other.example.com:443", "--json")

	require.NoError(t, err)
	var got controlproto.PolicyResult
	require.NoError(t, json.Unmarshal([]byte(out), &got))
	assert.True(t, got.Pending)
	assert.Equal(t, []string{"other.example.com:443"}, got.Entries)
}

// TestSandboxPolicy_JSONFailure_CarriesTheStableCode is the deliverable the
// integrating lane asked for (R-H233).
//
// A consumer built a pre-boot retry by matching on the error WORDING; it
// silently missed this failure, and rewording it would have broken them a
// second time just as silently. The token is what makes prose-matching
// unnecessary — so it must be on the FAILURE path, structurally, for every
// verb. Refs: MGIT-109, R-H233
func TestSandboxPolicy_JSONFailure_CarriesTheStableCode(t *testing.T) {
	verbs := map[string][]string{
		"set":    {"policy", "set", "--task", "MGIT-109", "--allow", "a.example:443", "--json"},
		"revoke": {"policy", "revoke", "--task", "MGIT-109", "--json"},
		"show":   {"policy", "show", "--task", "MGIT-109", "--json"},
	}
	for name, args := range verbs {
		t.Run(name, func(t *testing.T) {
			fc := &fakeSandboxClient{opErr: &model.EgressPolicyError{
				Code:   model.EgressFailureBootedDied,
				Reason: "sandbox: egress policy: the guest has exited or was killed",
			}}

			out, err := runSandbox(okConnect(fc), args...)

			require.Error(t, err, "a failed policy verb still exits non-zero")
			var got struct {
				Error     string `json:"error"`
				ErrorCode string `json:"error_code"`
			}
			require.NoError(t, json.Unmarshal([]byte(out), &got),
				"the failure itself must be machine-readable, not only the success")
			assert.Equal(t, model.EgressFailureBootedDied, got.ErrorCode)
			assert.NotEmpty(t, got.Error, "the prose still travels, for the human")
		})
	}
}

// TestSandboxPolicy_JSONFailure_UnclassifiedIsUnknownNotAGuess keeps the set
// closed. An error with no code must NOT be reported as the nearest of the
// three — a confident wrong answer is the defect this ticket is about, one
// layer down. Refs: MGIT-109, R-H233
func TestSandboxPolicy_JSONFailure_UnclassifiedIsUnknownNotAGuess(t *testing.T) {
	fc := &fakeSandboxClient{opErr: assertAnError{}}

	out, err := runSandbox(okConnect(fc), "policy", "show", "--task", "MGIT-109", "--json")

	require.Error(t, err)
	var got struct {
		ErrorCode string `json:"error_code"`
	}
	require.NoError(t, json.Unmarshal([]byte(out), &got))
	assert.Equal(t, model.EgressFailureUnknown, got.ErrorCode)
}

// TestSandboxPolicy_HumanFailure_CarriesTheCodeInline verifies the token is
// readable from a bare stderr line too, so a caller who never passes --json is
// not forced back onto prose-matching. Refs: MGIT-109, R-H233
func TestSandboxPolicy_HumanFailure_CarriesTheCodeInline(t *testing.T) {
	fc := &fakeSandboxClient{opErr: &model.EgressPolicyError{
		Code: model.EgressFailureVersionPredates, Reason: "sandbox: egress policy: relaunch it",
	}}

	_, err := runSandbox(okConnect(fc), "policy", "revoke", "--task", "MGIT-109")

	require.Error(t, err)
	assert.Contains(t, err.Error(), "["+model.EgressFailureVersionPredates+"]")
}

// TestSandboxPolicy_HelpDocumentsTheFailureCodes verifies the contract is
// documented where an integrator about to script the verb will read it — the
// same reason the established-flow note is repeated at each verb.
// Refs: MGIT-109, R-H233
func TestSandboxPolicy_HelpDocumentsTheFailureCodes(t *testing.T) {
	for _, verb := range []string{"set", "revoke", "show"} {
		t.Run(verb, func(t *testing.T) {
			out, err := runSandbox(okConnect(&fakeSandboxClient{}), "policy", verb, "--help")
			require.NoError(t, err)
			for _, code := range []string{
				model.EgressFailureNotBooted, model.EgressFailureBootedDied,
				model.EgressFailureVersionPredates, model.EgressFailureUnknown,
			} {
				assert.Contains(t, out, code)
			}
			assert.Contains(t, strings.ToLower(out), "match on the token",
				"the help must say the wording is not the contract")
		})
	}
}

// assertAnError is a plain error with no failure code, standing in for anything
// this build cannot classify.
type assertAnError struct{}

func (assertAnError) Error() string { return "something else went wrong" }
