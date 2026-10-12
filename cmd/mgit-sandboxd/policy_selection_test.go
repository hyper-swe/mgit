package main

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/hyper-swe/mgit/internal/model"
	"github.com/hyper-swe/mgit/internal/service"
)

// tagController is a policy controller that says which one it is, so a test
// can tell which enforcer a verb would reach.
type tagController struct{ tag string }

func (tagController) SetEgressPolicy(context.Context, string, []string, bool) (model.EgressPolicyChange, error) {
	return model.EgressPolicyChange{}, nil
}

func (tagController) EgressPolicy(context.Context, string) (model.EgressPolicyState, error) {
	return model.EgressPolicyState{}, nil
}

// A policy verb must act on the enforcer that actually holds the sandbox's
// policy. Where a platform controller exists (a libkrun build, whose
// enforcer is the VM child) it wins over the daemon's own egress runner,
// which enforces nothing for those sandboxes: routing a revoke to it would
// report success while the VM kept the old policy. With no platform
// controller the wired one serves, and with neither the verbs are reported
// unserved (a nil controller), never answered with a silent success.
// Refs: MGIT-72, MGIT-287, ADR-010, SEC-04
func TestSelectPolicyController_PlatformWinsThenWiredThenNone(t *testing.T) {
	platform, wired := tagController{"platform"}, tagController{"wired"}
	tests := []struct {
		name     string
		platform service.EgressPolicyController
		wired    egressWiring
		want     string
	}{
		{"platform_controller_wins_over_the_daemons_runner", platform, egressWiring{Policy: wired}, "platform"},
		{"wired_controller_serves_when_there_is_no_platform_one", nil, egressWiring{Policy: wired}, "wired"},
		{"neither_means_unserved", nil, egressWiring{}, ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ctrl := selectPolicyController(tt.platform, tt.wired)

			if tt.want == "" {
				assert.Nil(t, ctrl)
				return
			}
			require.NotNil(t, ctrl)
			got, ok := ctrl.(tagController)
			require.True(t, ok)
			assert.Equal(t, tt.want, got.tag)
		})
	}
}
