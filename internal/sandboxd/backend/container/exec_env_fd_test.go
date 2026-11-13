package container

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/hyper-swe/mgit/internal/model"
)

// launchedForExec starts a fake-runtime container and returns the manager and
// its id.
func launchedForExec(t *testing.T) (*fakeRunner, *Manager, string) {
	t.Helper()
	runner := &fakeRunner{}
	mgr := testManager(t, runner)
	info, err := mgr.Launch(context.Background(), containerOpts(t, model.NetworkModeNone))
	require.NoError(t, err)
	return runner, mgr, info.ID
}

// TestContainer_Exec_EnvReachesTheRuntimeOnTheDescriptor verifies the
// variables are what the runtime reads from descriptor 3.
func TestContainer_Exec_EnvReachesTheRuntimeOnTheDescriptor(t *testing.T) {
	runner, mgr, id := launchedForExec(t)

	_, err := mgr.Exec(context.Background(), id, model.ExecRequest{
		Command: []string{"true"},
		Env:     []string{"A=1", "MGIT_TEST_NAMED=" + execEnvMarker},
	})

	require.NoError(t, err)
	require.Len(t, runner.fds, 1)
	assert.Equal(t, "A=1\nMGIT_TEST_NAMED="+execEnvMarker+"\n", string(runner.fds[0]))
}

// TestContainer_Exec_NoEnvUsesNoDescriptor verifies an exec without variables
// is the plain invocation it always was.
func TestContainer_Exec_NoEnvUsesNoDescriptor(t *testing.T) {
	runner, mgr, id := launchedForExec(t)

	_, err := mgr.Exec(context.Background(), id, model.ExecRequest{Command: []string{"true"}})

	require.NoError(t, err)
	assert.Empty(t, runner.fds)
	assert.False(t, hasArg(runner.callsFor("exec")[0], "--env-file"))
}

// TestContainer_Exec_LineBreakInValueIsRefusedByName verifies a value the
// env-file format cannot carry is refused before the runtime is called, naming
// the variable and not showing the value.
func TestContainer_Exec_LineBreakInValueIsRefusedByName(t *testing.T) {
	runner, mgr, id := launchedForExec(t)

	_, err := mgr.Exec(context.Background(), id, model.ExecRequest{
		Command: []string{"true"},
		Env:     []string{"MGIT_TEST_NAMED=" + execEnvMarker + "\nB=2"},
	})

	require.Error(t, err)
	assert.Contains(t, err.Error(), "MGIT_TEST_NAMED")
	assert.NotContains(t, err.Error(), execEnvMarker)
	assert.Empty(t, runner.callsFor("exec"))
}

// TestContainer_Exec_UnreadableNamesAreRefused verifies names the env-file
// format would drop or alter are refused rather than silently changed.
func TestContainer_Exec_UnreadableNamesAreRefused(t *testing.T) {
	for _, env := range []string{"#K=v", " K=v", "K =v", "NOEQUALS"} {
		t.Run(env, func(t *testing.T) {
			runner, mgr, id := launchedForExec(t)

			_, err := mgr.Exec(context.Background(), id, model.ExecRequest{Command: []string{"true"}, Env: []string{env}})

			require.Error(t, err)
			assert.Empty(t, runner.callsFor("exec"))
		})
	}
}

// TestContainer_Exec_EntryWithoutEqualsIsNotEchoed verifies an entry that is
// only a value, with no name part, never appears in the refusal.
func TestContainer_Exec_EntryWithoutEqualsIsNotEchoed(t *testing.T) {
	runner, mgr, id := launchedForExec(t)

	_, err := mgr.Exec(context.Background(), id, model.ExecRequest{Command: []string{"true"}, Env: []string{execEnvMarker}})

	require.Error(t, err)
	assert.NotContains(t, err.Error(), execEnvMarker)
	assert.Empty(t, runner.callsFor("exec"))
}
