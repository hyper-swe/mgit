package main

import (
	"bytes"
	"errors"
	"strings"
	"testing"

	"github.com/spf13/cobra"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// A RUNTIME failure must not print the usage/flag dump.
//
// `mgit work` failing to materialize a worktree emitted the error and then
// twenty lines of flags, which pushes the one line that matters off the top of
// a terminal and implies the user mistyped something. Usage belongs to usage
// errors. Refs: MGIT-157
func TestRootCmd_RuntimeFailure_DoesNotPrintTheUsageDump(t *testing.T) {
	root := rootCmd()
	root.SetArgs([]string{"boom"})
	root.AddCommand(&cobra.Command{
		Use:  "boom",
		Args: cobra.NoArgs,
		RunE: func(*cobra.Command, []string) error { return errors.New("the disk caught fire") },
	})
	var out, errOut bytes.Buffer
	root.SetOut(&out)
	root.SetErr(&errOut)

	err := root.Execute()
	require.Error(t, err)

	combined := out.String() + errOut.String()
	assert.NotContains(t, combined, "Usage:",
		"a runtime failure printed the usage dump, burying the error")
	assert.NotContains(t, combined, "--help",
		"a runtime failure printed the flag list")
}

// A genuine USAGE error still gets usage: that is what it is for. Refs: MGIT-157
func TestRootCmd_UsageError_StillPrintsUsage(t *testing.T) {
	root := rootCmd()
	root.SetArgs([]string{"work"}) // missing the required path argument
	var out, errOut bytes.Buffer
	root.SetOut(&out)
	root.SetErr(&errOut)

	err := root.Execute()
	require.Error(t, err)

	combined := out.String() + errOut.String()
	assert.True(t, strings.Contains(combined, "Usage:"),
		"a real usage error must still show usage; got: %s", combined)
}
