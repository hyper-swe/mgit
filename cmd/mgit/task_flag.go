package main

import "github.com/spf13/cobra"

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
