package main

import (
	"fmt"
	"path/filepath"
)

// forkCommand is how a new line is forked from a good commit when the work
// happens in a task worktree. The working discipline written into every
// worktree names the same command (internal/agentadapter). Refs: MGIT-82
const forkCommand = "mgit work <new-path> --task-id <new-task-id> --base <good-commit>"

// errBranchSwitchInWorktree refuses a branch switch inside a task worktree and
// says how to fork instead.
//
// A linked worktree is bound to one branch for its lifetime: switching would
// move the shared parent's HEAD (MGIT-24). The refusal used to stop there,
// while the guidance mgit writes into the same worktree told the agent to fork
// with `mgit checkout -b`, so the course-correction step it prescribed failed
// with nothing to do next. A new line from a good commit is a new task
// worktree forked at that commit, made from the project root. Refs: MGIT-82, MGIT-24
func errBranchSwitchInWorktree(app *App) error {
	return fmt.Errorf("cannot switch branches in a linked worktree (bound to task %s); "+
		"to fork a new line from a good commit, run from the project root (%s): %s",
		app.BoundTask, filepath.Dir(app.storeDir), forkCommand)
}
