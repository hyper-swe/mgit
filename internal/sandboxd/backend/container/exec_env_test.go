package container

import (
	"context"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/hyper-swe/mgit/internal/model"
)

const execEnvMarker = "zz-marker-5e21b0d4"

// TestContainer_Exec_EnvValuesAreNotOnTheRuntimeCommandLine verifies a
// per-exec variable reaches the runtime without its value being a command-line
// argument of the runtime process, and that the runtime is told where the
// value arrives instead.
func TestContainer_Exec_EnvValuesAreNotOnTheRuntimeCommandLine(t *testing.T) {
	runner := &fakeRunner{}
	mgr := testManager(t, runner)
	ctx := context.Background()
	info, err := mgr.Launch(ctx, containerOpts(t, model.NetworkModeNone))
	require.NoError(t, err)

	_, err = mgr.Exec(ctx, info.ID, model.ExecRequest{
		Command: []string{"true"},
		Env:     []string{"MGIT_TEST_NAMED=" + execEnvMarker},
	})

	require.NoError(t, err)
	execCalls := runner.callsFor("exec")
	require.Len(t, execCalls, 1)
	assert.NotContains(t, strings.Join(execCalls[0], " "), execEnvMarker)
	assert.True(t, hasArg(execCalls[0], "--env-file"), "the runtime reads the variables from a file descriptor")
}
