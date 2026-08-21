package service

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/hyper-swe/mgit/internal/model"
)

// Two verbs answered "where am I" differently, and that was an accident of two
// code paths rather than a decision.
//
//	mgit run          -> the worktree (it sends the host cwd, which the
//	                     identical-path mount makes valid in the guest)
//	mgit sandbox exec -> "/"          (it sent no Dir, so the child inherited
//	                     PID 1's cwd)
//
// The sandbox is BOUND to a task worktree; that worktree is the only directory
// the binding implies, and "/" is not a decision anyone made. Defaulting here —
// in the service, not in one CLI verb — fixes every client at once, including
// MCP and anything else that speaks the control plane. Refs: MGIT-152
func TestExec_WithoutADir_DefaultsToTheTasksWorktree(t *testing.T) {
	mgr := &fakeSandboxManager{}
	svc := newSvc(t, mgr, &fakeEventAppender{})
	wt := t.TempDir()

	_, err := svc.Register(context.Background(), regOpts("MGIT-152", wt))
	require.NoError(t, err)

	_, err = svc.Exec(context.Background(), "MGIT-152", model.ExecRequest{Command: []string{"pwd"}})
	require.NoError(t, err)
	assert.Equal(t, wt, mgr.lastExecReq.Dir,
		"an exec with no Dir must land in the task's worktree, not wherever PID 1 stands")
}

// A caller that names a directory keeps it. `mgit run` sends the host cwd,
// which may be a SUBDIRECTORY of the worktree, and silently promoting that to
// the worktree root would move the user's shell out from under them.
// Refs: MGIT-152
func TestExec_WithAnExplicitDir_IsLeftAlone(t *testing.T) {
	mgr := &fakeSandboxManager{}
	svc := newSvc(t, mgr, &fakeEventAppender{})
	wt := t.TempDir()

	_, err := svc.Register(context.Background(), regOpts("MGIT-152", wt))
	require.NoError(t, err)

	sub := wt + "/internal/store"
	_, err = svc.Exec(context.Background(), "MGIT-152",
		model.ExecRequest{Command: []string{"pwd"}, Dir: sub})
	require.NoError(t, err)
	assert.Equal(t, sub, mgr.lastExecReq.Dir, "an explicit Dir is the caller's decision")
}

// There is no "sandbox without a worktree" case to guard: registration
// REFUSES an empty worktree_path, so the default above can never be reaching
// for something absent. Asserted rather than assumed — a fallback written for
// a state the system cannot enter is dead code that reads like caution.
// Refs: MGIT-152, FR-17.1
func TestRegister_WithoutAWorktree_IsRefused(t *testing.T) {
	svc := newSvc(t, &fakeSandboxManager{}, &fakeEventAppender{})
	opts := regOpts("MGIT-152", t.TempDir())
	opts.WorktreePath = ""

	_, err := svc.Register(context.Background(), opts)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "worktree_path")
}
