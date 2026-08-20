package sandboxd

import (
	"context"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/hyper-swe/mgit/internal/model"
)

// The refusal an operator meets when this build has no live egress enforcer
// must describe CONTAINMENT, not wire plumbing.
//
// It used to read "controlproto kind 0x51 not served by this daemon", which
// tells an operator that their command was not served and nothing about the
// fact that matters before running untrusted code: this backend enforces no
// live allowlist, so there is no policy here to show or change. Those are
// different facts, and only the second one is actionable. Refs: MGIT-111, MGIT-104, SEC-04
func TestPolicyVerbs_Unserved_NameTheContainmentFactNotTheWireKind(t *testing.T) {
	svc := newDrainRecorder("MGIT-111")
	svc.tasks[0].Backend = model.BackendVZF
	svc.tasks[0].NetworkMode = model.NetworkModeNone

	cfg, _ := testConfig(t, newFakeManager())
	cfg.Service = svc
	cfg.Policy = nil // no live enforcer on this build

	d, err := New(cfg)
	require.NoError(t, err)

	for _, tc := range []struct {
		name string
		run  func() error
	}{
		{name: "set", run: func() error { return d.policyUnservedReason(context.Background(), "MGIT-111") }},
		{name: "show", run: func() error { return d.policyUnservedReason(context.Background(), "MGIT-111") }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := tc.run()
			require.Error(t, err)
			msg := err.Error()

			// The wire kind is an implementation detail and must not be the message.
			assert.NotContains(t, msg, "controlproto",
				"the operator is being shown wire plumbing instead of a containment fact")
			assert.NotContains(t, msg, "0x5",
				"a frame tag number tells an operator nothing they can act on")

			// It must name the containment fact and the backend it applies to.
			lower := strings.ToLower(msg)
			assert.Contains(t, lower, "allowlist", "the message must say what cannot be enforced")
			assert.Contains(t, lower, model.BackendVZF, "the message must name the backend it is describing")
			assert.Contains(t, lower, "enforce", "the message must state the enforcement fact")
		})
	}
}

// When the sandbox cannot be resolved the refusal still has to be honest: it
// says what it could not determine rather than inventing a backend.
// Refs: MGIT-111
func TestPolicyVerbs_Unserved_UnknownTask_StillAvoidsWirePlumbing(t *testing.T) {
	cfg, _ := testConfig(t, newFakeManager())
	cfg.Service = newDrainRecorder() // knows no tasks
	cfg.Policy = nil

	d, err := New(cfg)
	require.NoError(t, err)

	err = d.policyUnservedReason(context.Background(), "NO-SUCH-TASK")
	require.Error(t, err)
	assert.NotContains(t, err.Error(), "controlproto")
	assert.Contains(t, strings.ToLower(err.Error()), "allowlist")
}
