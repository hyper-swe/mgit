package main

import (
	"strings"
	"testing"

	"github.com/spf13/cobra"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Every sandbox verb's usage line names the flag that is documented,
// --task-id. The hidden alias --task still works, but a usage line that
// shows it sends people to a spelling the help does not list. Refs: FEAT-6.115
func TestSandboxUsageLinesNameTaskID(t *testing.T) {
	root := newSandboxCmd(okConnect(&fakeSandboxClient{}))
	var walk func(c *cobra.Command)
	seen := 0
	walk = func(c *cobra.Command) {
		if strings.Contains(c.Use, "--task") {
			seen++
			assert.Contains(t, c.Use, "--task-id", "%q names the flag that works", c.CommandPath())
			assert.NotRegexp(t, `--task\b[^-]`, c.Use+" ", "%q must not show the bare --task alias", c.CommandPath())
		}
		for _, sub := range c.Commands() {
			walk(sub)
		}
	}
	walk(root)
	require.Greater(t, seen, 5, "the walk found the sandbox verbs that take a task")
}
