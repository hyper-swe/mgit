package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"sort"
	"strings"

	"github.com/hyper-swe/mgit/internal/model"
	gitstore "github.com/hyper-swe/mgit/internal/store/git"
)

// describeDaemon names the daemon a client talks to, for a refusal that must
// say which registry it consulted (R-H300 rule 2). A client that cannot name
// itself is described as such rather than left out. Refs: MGIT-196
func describeDaemon(cl sandboxClient) string {
	root, socket := cl.DaemonIdentity()
	if root == "" {
		return "the daemon this directory resolves to"
	}
	return fmt.Sprintf("the daemon of repository %s (socket %s)", root, socket)
}

// describeHoldings renders what a daemon's registry holds — every binding,
// task by task, with its worktree and state — so a reader sees at once whether
// the sandbox they mean is here under another path, or not here at all.
// Refs: MGIT-196
func describeHoldings(list []model.SandboxInfo) string {
	if len(list) == 0 {
		return "no sandboxes"
	}
	parts := make([]string, 0, len(list))
	for _, sb := range list {
		parts = append(parts, fmt.Sprintf("%s at %s (%s)", sb.TaskID, sb.WorktreePath, sb.State))
	}
	sort.Strings(parts)
	noun := "sandboxes"
	if len(list) == 1 {
		noun = "sandbox"
	}
	return fmt.Sprintf("%d %s: %s", len(list), noun, strings.Join(parts, ", "))
}

// unboundError is the refusal `mgit run` and `mgit doctor` share when no
// registered sandbox covers dir: which daemon was asked, what it holds, and
// what to do. Its first line keeps the wording every hook and script has read
// since MGIT-11.11.5. Refs: MGIT-196, NFR-17.6
func unboundError(cl sandboxClient, dir string, list []model.SandboxInfo) error {
	return fmt.Errorf("no sandbox bound for %s\n"+
		"  asked: %s, which has %s\n"+
		"  a sandbox launched from another repository, or from another spelling of this repository's path, "+
		"is served by that daemon: `mgit sandbox daemons` lists every daemon on this host\n"+
		"  remedy: run from inside a registered worktree, or `mgit sandbox launch --task-id <id> --worktree %s` "+
		"from the repository that should own it (commands never run on the host)",
		dir, describeDaemon(cl), describeHoldings(list), dir)
}

// explainNotFound wraps a daemon's "sandbox not found" with the daemon that
// said so and what it holds, keeping the sentinel intact for callers that
// test for it. Any other error is returned unchanged. Refs: MGIT-196
func explainNotFound(ctx context.Context, cl sandboxClient, err error) error {
	if err == nil || !errors.Is(err, model.ErrSandboxNotFound) {
		return err
	}
	var holdings string
	if list, lerr := cl.List(ctx); lerr == nil {
		holdings = describeHoldings(list)
	} else {
		holdings = fmt.Sprintf("a registry that could not be listed (%v)", lerr)
	}
	return fmt.Errorf("%w\n  asked: %s, which has %s\n"+
		"  a task launched from another repository, or from another spelling of this repository's path, "+
		"lives in that daemon: `mgit sandbox daemons` lists every daemon on this host",
		err, describeDaemon(cl), holdings)
}

// recordSandboxOwner writes the owner file into a just-registered sandbox's
// worktree, so verbs run from inside it reach the daemon that holds the
// binding (MGIT-196). The sandbox is registered either way; a record that
// could not be written is reported, never swallowed. A client that cannot name
// its daemon (a test double) records nothing. Refs: MGIT-196
func recordSandboxOwner(w io.Writer, cl sandboxClient, info *model.SandboxInfo) {
	root, _ := cl.DaemonIdentity()
	if root == "" || info == nil || info.WorktreePath == "" {
		return
	}
	err := gitstore.WriteSandboxOwner(info.WorktreePath, gitstore.SandboxOwner{RepoRoot: root, Task: info.TaskID})
	if err != nil {
		_, _ = fmt.Fprintf(w, "warning: could not record the owning repository in %s/.mgit/sandbox-owner (%v): "+
			"sandbox verbs run from inside that directory will not find this sandbox until it is recorded\n",
			info.WorktreePath, err)
	}
}

// storelessMgitError names a directory whose .mgit is neither a store, nor a
// linked worktree's marker, nor a sandbox worktree that names its owner: the
// decoration `mgit sandbox launch --worktree` left behind before the owner was
// recorded. Opening it as an empty repository of its own is the defect.
// Refs: MGIT-196
func storelessMgitError(root string) error {
	return fmt.Errorf("not an mgit repository: %s/.mgit holds no store and names no owning repository — "+
		"it carries only the agent files `mgit sandbox launch --worktree` writes (a launch from before the owner "+
		"was recorded); run this command from the repository that launched the sandbox, or record the owner by "+
		"launching again from there (`mgit sandbox remove --task-id <id>` first if the task is still bound)", root)
}

// clearSandboxOwner retires the owner record launch wrote for task once its
// binding is gone; a record that could not be cleared is said, never
// swallowed. Refs: MGIT-196
func clearSandboxOwner(w io.Writer, info *model.SandboxInfo, task string) {
	if info == nil || info.WorktreePath == "" {
		return
	}
	if err := gitstore.ClearSandboxOwner(info.WorktreePath, task); err != nil {
		_, _ = fmt.Fprintf(w, "warning: the owner record in %s/.mgit was not cleared (%v)\n", info.WorktreePath, err)
	}
}
