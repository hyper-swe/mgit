package main

import (
	"testing"

	"github.com/spf13/cobra"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestRefusePaths_NoArguments_Accepted(t *testing.T) {
	assert.NoError(t, refusePaths("commit", commitPathRemedy)(&cobra.Command{}, nil))
}

func TestRefusePaths_AnyArgument_RefusedNamingItAndTheRemedy(t *testing.T) {
	cmd := &cobra.Command{}
	err := refusePaths("commit", commitPathRemedy)(cmd, []string{"pkg", "b.go"})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "mgit commit does not take a path")
	assert.Contains(t, err.Error(), "pkg b.go")
	assert.Contains(t, err.Error(), "mgit restore --staged <path>")
	assert.True(t, cmd.SilenceUsage, "the usage table must not bury the remedy")
}

// Every verb that drops a path today carries the check. Refs: MGIT-282
func TestCommitStatusDiff_RefuseAPath(t *testing.T) {
	for name, cmd := range map[string]*cobra.Command{"commit": commitCmd(), "status": statusCmd(), "diff": diffCmd()} {
		require.NotNil(t, cmd.Args, name)
		assert.Error(t, cmd.Args(cmd, []string{"pkg"}), name)
		assert.NoError(t, cmd.Args(cmd, nil), name)
	}
}
