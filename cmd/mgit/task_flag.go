package main

import (
	"strings"

	"github.com/hyper-swe/mgit/internal/model"
	"github.com/spf13/cobra"
)

// canonicalTaskFlag is the single documented spelling for the task-ID flag
// across every mgit command that takes one.
const canonicalTaskFlag = "task-id"

// legacyTaskFlag is the back-compat alias kept hidden so ids resolve no matter
// which spelling a user or agent guesses.
const legacyTaskFlag = "task"

// bindTaskIDFlag registers the canonical `--task-id` flag and a hidden `--task`
// alias, both bound to the same target string. Either spelling resolves to the
// identical value; if both are supplied on one invocation, pflag applies
// last-wins. Existing `if taskID == ""` required-flag checks keep working
// unchanged because both names share the target.
//
// Refs: MGIT-37
func bindTaskIDFlag(cmd *cobra.Command, target *string, usage string) {
	cmd.Flags().StringVar(target, canonicalTaskFlag, "", usage)
	cmd.Flags().StringVar(target, legacyTaskFlag, "", "alias for --task-id")
	// Hide the alias so only the canonical spelling appears in help output.
	_ = cmd.Flags().MarkHidden(legacyTaskFlag)
}

// addTaskIDHelp states, in the help of every command under root that takes
// the task flag, what a task id looks like and shows one in that command's
// own usage. The format was only in the refusal error before; help is where
// a person looks first. Refs: FEAT-6.115
func addTaskIDHelp(root *cobra.Command) {
	for _, c := range root.Commands() {
		addTaskIDHelp(c)
	}
	if c := root; c.Flags().Lookup(canonicalTaskFlag) != nil {
		body := c.Long
		if body == "" {
			body = c.Short
		}
		example := "mgit " + strings.TrimPrefix(c.CommandPath(), "mgit ")
		if i := strings.Index(c.Use, " "); i >= 0 {
			example += c.Use[i:]
		}
		example = strings.ReplaceAll(example, "<id>", "MGIT-1.2")
		c.Long = body + "\n\nTask id format: " + model.TaskIDGrammar() + "\n\nExample:\n  " + example
	}
}
