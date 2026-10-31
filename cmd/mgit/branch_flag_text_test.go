package main

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// --branch binds an EXISTING branch, or the task's own branch; mgit creates
// only task/<id>. The help for both verbs and the README row say so, because the
// flag's old help ("Branch name") read as "name the new branch". Refs: MGIT-127
func TestBranchFlag_HelpAndReadmeStateTheRule(t *testing.T) {
	for _, args := range [][]string{{"work", "--help"}, {"worktree", "add", "--help"}} {
		out, errOut, err := runCLICap(t, args...)
		require.NoError(t, err, errOut)
		assert.Contains(t, out+errOut, "existing branch", "%v help must say --branch takes an existing branch", args)
		assert.Contains(t, out+errOut, "task/<task-id>", "%v help must name the one branch mgit creates", args)
	}

	_, thisFile, _, ok := runtime.Caller(0)
	require.True(t, ok)
	readme, err := os.ReadFile(filepath.Join(filepath.Dir(thisFile), "..", "..", "README.md")) //nolint:gosec // repository file located from this test's path
	require.NoError(t, err)
	assert.Contains(t, string(readme), "an existing branch, or `task/<id>`", "the README row for worktree add states the --branch rule")
}
