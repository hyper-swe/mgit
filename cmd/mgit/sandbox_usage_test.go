package main

import (
	"regexp"
	"testing"

	"github.com/hyper-swe/mgit/internal/model"
	"github.com/spf13/cobra"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

var usageTaskFlagRE = regexp.MustCompile(`--(task[A-Za-z0-9-]*)`)

// usageTaskFlagProblem returns why a command's usage line misnames its task
// flag, or "". The expected spelling comes from the REGISTERED flag
// (canonicalTaskFlag), never from a literal here, so renaming the flag
// without its usage lines fails, and a usage line that names a flag the
// command does not register or hides fails too.
func usageTaskFlagProblem(c *cobra.Command) string {
	m := usageTaskFlagRE.FindStringSubmatch(c.Use)
	if m == nil {
		return ""
	}
	if m[1] != canonicalTaskFlag {
		return "usage names --" + m[1] + ", the flag is --" + canonicalTaskFlag
	}
	f := c.Flags().Lookup(m[1])
	switch {
	case f == nil:
		return "usage names --" + m[1] + ", which the command does not register"
	case f.Hidden:
		return "usage names --" + m[1] + ", which is hidden from help"
	}
	return ""
}

func walkCommands(c *cobra.Command, fn func(*cobra.Command)) {
	fn(c)
	for _, sub := range c.Commands() {
		walkCommands(sub, fn)
	}
}

// Every sandbox verb's usage line names the registered, documented task
// flag. The hidden alias still works, but a usage line that shows it (or a
// renamed flag's old name) sends people to a spelling the help does not
// list. Refs: FEAT-6.115
func TestSandboxUsageLinesNameTheRegisteredTaskFlag(t *testing.T) {
	seen := 0
	walkCommands(newSandboxCmd(okConnect(&fakeSandboxClient{})), func(c *cobra.Command) {
		if usageTaskFlagRE.MatchString(c.Use) {
			seen++
			assert.Empty(t, usageTaskFlagProblem(c), c.CommandPath())
		}
	})
	require.Greater(t, seen, 5, "the walk found the sandbox verbs that take a task")
}

// The check itself must fail for a usage line that names a dead flag, such
// as the old name after a rename, or the hidden alias.
func TestUsageCheckCatchesADeadFlagName(t *testing.T) {
	for _, use := range []string{"exec --task-ident <id>", "exec --task <id>"} {
		c := &cobra.Command{Use: use}
		var task string
		bindTaskIDFlag(c, &task, "task")
		assert.NotEmpty(t, usageTaskFlagProblem(c), use)
	}
	c := &cobra.Command{Use: "exec --" + canonicalTaskFlag + " <id>"}
	var task string
	bindTaskIDFlag(c, &task, "task")
	assert.Empty(t, usageTaskFlagProblem(c))
}

// Every sandbox verb that takes a task says in its help what a task id
// looks like and shows one in a command, not only in the refusal error.
func TestSandboxHelpStatesTheTaskIDFormatWithAnExample(t *testing.T) {
	seen := 0
	walkCommands(newSandboxCmd(okConnect(&fakeSandboxClient{})), func(c *cobra.Command) {
		if c.Flags().Lookup(canonicalTaskFlag) == nil {
			return
		}
		seen++
		help := c.Long
		assert.Contains(t, help, model.TaskIDGrammar(), c.CommandPath()+": the format")
		assert.Contains(t, help, "Example", c.CommandPath())
		assert.Contains(t, help, "--"+canonicalTaskFlag+" MGIT-1.2", c.CommandPath()+": an example id in a command")
	})
	require.Greater(t, seen, 5)
}

func TestSandboxHelpOutputCarriesTheFormat(t *testing.T) {
	out, err := runSandbox(okConnect(&fakeSandboxClient{}), "exec", "--help")
	require.NoError(t, err)
	assert.Contains(t, out, model.TaskIDGrammar())
	assert.Contains(t, out, "mgit sandbox exec --"+canonicalTaskFlag+" MGIT-1.2")
}
