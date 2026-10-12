package main

import (
	"fmt"
	"io"
	"strings"

	"github.com/hyper-swe/mgit/internal/model"
)

// reportUncommitted names, on w, every path the checkout had not committed
// when a NEW task's worktree was created, and says whether it went into the
// task's base. By default it did not: the base is git's committed tree, and
// the files stay uncommitted in this checkout. With --include-uncommitted it
// did, and the user must know exactly which files a consumer of the task may
// now land. Nothing is printed when there was nothing uncommitted.
// Refs: MGIT-283, ADR-008 §2
func reportUncommitted(w io.Writer, wt *model.WorktreeInfo) {
	if len(wt.UncommittedPaths) == 0 {
		return
	}
	list := "  " + strings.Join(wt.UncommittedPaths, "\n  ")
	if wt.UncommittedIncluded {
		_, _ = fmt.Fprintf(w, "Captured into the task's base (uncommitted in this checkout, by --include-uncommitted):\n%s\n", list)
		return
	}
	_, _ = fmt.Fprintf(w, "Left out of the task's base (uncommitted in this checkout; the task starts from git's "+
		"committed tree, pass --include-uncommitted to capture them):\n%s\n", list)
}
