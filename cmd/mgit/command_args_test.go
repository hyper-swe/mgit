package main

import (
	"bytes"
	"strings"
	"testing"

	"github.com/spf13/cobra"
	"github.com/stretchr/testify/assert"
)

// takesPositionals reports whether a command's Use line documents positional
// arguments: anything after the command's own name other than flags, such as
// "add [paths...]", "branch [name]" or "run -- <command>". A placeholder that
// directly follows a flag ("--task <id>") is that flag's value, not a
// positional argument.
func takesPositionals(cmd *cobra.Command) bool {
	fields := strings.Fields(cmd.Use)
	for i := 1; i < len(fields); i++ {
		f := fields[i]
		if f == "--" {
			return true // everything after "--" is passed through
		}
		if strings.HasPrefix(f, "--") || strings.HasPrefix(f, "[--") {
			if !strings.Contains(f, "=") && !strings.HasSuffix(f, "]") && i+1 < len(fields) &&
				strings.HasPrefix(fields[i+1], "<") {
				i++ // the flag's value
			}
			continue
		}
		if strings.ContainsAny(f, "[<") {
			return true
		}
	}
	return false
}

// THE WHOLE COMMAND TREE (MGIT-284, the founder's rule for v0.7.1): every
// runnable command that documents no positional argument refuses one. A
// command that declared no Args rule accepted a stray argument and silently
// dropped it: `mgit squash --to-git <path>` exported the whole task. Walking
// the tree from the root means a command added later without a rule fails
// here, not in a user's hands. Refs: MGIT-284, MGIT-282
func TestEveryCommand_DocumentsItsArgumentsOrRefusesThem(t *testing.T) {
	var walk func(cmd *cobra.Command)
	walk = func(cmd *cobra.Command) {
		for _, c := range cmd.Commands() {
			walk(c)
		}
		if !cmd.Runnable() || cmd.Hidden || cmd.Name() == "help" || cmd.Name() == "completion" {
			return
		}
		if takesPositionals(cmd) {
			return
		}
		path := cmd.CommandPath()
		if !assert.NotNil(t, cmd.Args, "%s documents no positional argument but declares no Args rule", path) {
			return
		}
		assert.Error(t, cmd.Args(cmd, []string{"stray"}), "%s must refuse an argument it does not take", path)
		assert.NoError(t, cmd.Args(cmd, nil), "%s must still run with no argument", path)
	}
	walk(rootCmd())
}

// The detector the walk relies on is held to fixtures, so a usage line it
// misreads cannot quietly exempt a command.
func TestTakesPositionals_ReadsUsageLines(t *testing.T) {
	for use, want := range map[string]bool{
		"squash":                  false,
		"status":                  false,
		"add [paths...]":          true,
		"branch [name]":           true,
		"sandbox base from <ref>": true,
		"run [--env KEY=VALUE]... -- <command> [args...]":      true,
		"restore [file] [commit] | restore --staged <path>...": true,
		"set --task <id> --allow <host:port> [--allow ...]":    false,
		"revoke --task <id>": false,
		"show --task <id>":   false,
	} {
		assert.Equal(t, want, takesPositionals(&cobra.Command{Use: use}), use)
	}
}

// A command that silences cobra's error printing still tells the user why
// its arguments were refused, once; one that does not is left to cobra, so
// nothing is printed twice. Refs: MGIT-284
func TestAnnounceArgErrors_PrintsARefusalOnlyWhereCobraWouldNot(t *testing.T) {
	var silentErr, loudErr bytes.Buffer
	silent := &cobra.Command{Use: "silent", Args: cobra.NoArgs, SilenceErrors: true, Run: func(*cobra.Command, []string) {}}
	loud := &cobra.Command{Use: "loud", Args: cobra.NoArgs, Run: func(*cobra.Command, []string) {}}
	silent.SetErr(&silentErr)
	loud.SetErr(&loudErr)
	root := &cobra.Command{Use: "root"}
	root.AddCommand(silent, loud)
	announceArgErrors(root)

	assert.Error(t, silent.Args(silent, []string{"zzstray"}))
	assert.Contains(t, silentErr.String(), "Error:")
	assert.Contains(t, silentErr.String(), "zzstray")
	assert.Error(t, loud.Args(loud, []string{"zzstray"}))
	assert.Empty(t, loudErr.String(), "cobra prints a non-silenced command's error itself")
	before := silentErr.Len()
	assert.NoError(t, silent.Args(silent, nil))
	assert.Equal(t, before, silentErr.Len(), "an accepted call prints nothing")
}
