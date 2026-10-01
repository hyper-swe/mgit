package main

import (
	"fmt"
	"strings"

	"github.com/spf13/cobra"
)

// refusePaths is the positional-argument check for a verb that does not
// scope to a path: commit, status and diff.
//
// Each of them declared no arguments, so the parser accepted a path and the
// verb dropped it. `mgit commit -m x pkg`, the git habit, recorded everything
// staged rather than pkg, and nothing said so (MGIT-282). A verb that cannot
// honor a path must not pretend to: the path is refused before anything is
// recorded or printed, naming what was given and the way to do what was meant.
// The usage table is not printed, so the remedy is the last thing read.
// Path scoping itself is a separate decision (MGIT-278). Refs: MGIT-282
func refusePaths(verb, remedy string) cobra.PositionalArgs {
	return func(cmd *cobra.Command, args []string) error {
		if len(args) == 0 {
			return nil
		}
		cmd.SilenceUsage = true
		return fmt.Errorf("mgit %s does not take a path (given: %s); it acts on everything, "+
			"not on the path named, so the path is refused rather than ignored. %s",
			verb, strings.Join(args, " "), remedy)
	}
}

// The remedy each refusal gives. Refs: MGIT-282
const (
	commitPathRemedy = "Stage only what you want to record, then commit: " +
		"`mgit restore --staged <path>` unstages what should not go in, " +
		"`mgit add <path>` stages what should, and `mgit status` shows what is staged."
	statusPathRemedy = "Run `mgit status` without a path; it reports the whole working tree."
	diffPathRemedy   = "Run `mgit diff` without a path, with --task-id <id> or --from/--to."
)
