package service

import (
	"context"
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/hyper-swe/mgit/internal/model"
)

// A guest that was reached and then stopped answering is a dead sandbox:
// audited as guest_died, reported as dead, refused at once on every later
// exec with the remedy — never `running` plus a dial timeout per command
// (MGIT-99, reproduced live: 15 s and the same advisory, forever).
func TestExec_LostServingAfterTheGuestAnswered_MarksTheSandboxDead(t *testing.T) {
	mgr := &fakeSandboxManager{execResult: &model.ExecResult{ExitCode: 0}}
	ev := &fakeEventAppender{}
	svc := newSvc(t, mgr, ev)
	opts := regOpts("MGIT-99", "/work/a")
	opts.MemoryMB = 512
	_, err := svc.Register(context.Background(), opts)
	require.NoError(t, err)
	_, err = svc.Exec(context.Background(), "MGIT-99", model.ExecRequest{Command: []string{"true"}})
	require.NoError(t, err, "the guest answers once")

	mgr.execErr = errors.New("libkrun exec: guest exec: read frame: EOF")
	_, err = svc.Exec(context.Background(), "MGIT-99", model.ExecRequest{Command: []string{"dd"}})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "read frame: EOF", "the killing command's own failure is reported as it happened")

	st, err := svc.Status(context.Background(), "MGIT-99")
	require.NoError(t, err)
	assert.Equal(t, model.StateDead, st.State, "status must not say running about a guest nobody can reach")
	assert.Equal(t, model.EventGuestDied, ev.types()[len(ev.types())-1], "the death is on the audit trail")

	execsBefore := mgr.execs
	_, err = svc.Exec(context.Background(), "MGIT-99", model.ExecRequest{Command: []string{"echo"}})
	require.ErrorIs(t, err, model.ErrGuestDead)
	assert.Equal(t, execsBefore, mgr.execs, "a dead guest is refused before any dial — no 15 s timeout per command")
	for _, want := range []string{"mgit sandbox remove MGIT-99 --force", "mgit sandbox launch --task-id MGIT-99", "--memory-mb 512", "read frame: EOF"} {
		assert.Contains(t, err.Error(), want, "the refusal names the remedy, keeps the declared memory and quotes the cause")
	}

	listed, err := svc.List(context.Background())
	require.NoError(t, err)
	require.Len(t, listed, 1)
	assert.Equal(t, model.StateDead, listed[0].State)

	// The sibling verbs doctor's guest rows and the operator use refuse the
	// same way, at once: they had each waited out the 15 s dial timeout.
	_, err = svc.VerifyGuestView(context.Background(), "MGIT-99")
	assert.ErrorIs(t, err, model.ErrGuestDead)
	_, err = svc.SyncWorktree(context.Background(), "MGIT-99", model.WorktreeSyncOptions{})
	assert.ErrorIs(t, err, model.ErrGuestDead)
	assert.Equal(t, execsBefore, mgr.execs, "none of them dialed the guest")

	require.NoError(t, svc.Remove(context.Background(), "MGIT-99", true), "remove tears a dead sandbox down")
	assert.Equal(t, 1, mgr.stops, "the VM process that outlived its guest is stopped")
	assert.Equal(t, model.EventDestroyed, ev.types()[len(ev.types())-1])
	_, err = svc.Status(context.Background(), "MGIT-99")
	assert.ErrorIs(t, err, model.ErrSandboxNotFound)
}

// Only a reached-then-lost signature is death. The host's own patience
// (a deadline) and the caller's own cancellation are not statements about
// the guest, and a plain failing command is just that.
func TestExec_OtherFailures_DoNotMarkTheSandboxDead(t *testing.T) {
	tests := []struct {
		name   string
		err    error
		cancel bool
	}{
		{"a_read_deadline", errors.New("read: i/o timeout"), false},
		{"a_failing_command", errors.New("sandbox exec: exit status 2"), false},
		{"the_callers_cancellation", errors.New("use of closed network connection"), true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			mgr := &fakeSandboxManager{execResult: &model.ExecResult{}}
			ev := &fakeEventAppender{}
			svc := newSvc(t, mgr, ev)
			_, err := svc.Register(context.Background(), regOpts("MGIT-1", "/work/a"))
			require.NoError(t, err)
			_, err = svc.Exec(context.Background(), "MGIT-1", model.ExecRequest{Command: []string{"true"}})
			require.NoError(t, err)
			ctx, cancel := context.WithCancel(context.Background())
			if tt.cancel {
				cancel()
			} else {
				defer cancel()
			}
			mgr.execErr = tt.err
			_, err = svc.Exec(ctx, "MGIT-1", model.ExecRequest{Command: []string{"x"}})
			require.Error(t, err)
			st, serr := svc.Status(context.Background(), "MGIT-1")
			require.NoError(t, serr)
			assert.Equal(t, model.StateRunning, st.State)
			assert.NotContains(t, ev.types(), model.EventGuestDied)
		})
	}
}

// A row recorded dead by the previous daemon comes back dead: adopted (its
// VM process may still be there to stop), refused at once on exec, never
// re-reported as running by a fresh daemon that could not know better.
func TestRehydrate_RecordedDead_ComesBackDeadAndRefused(t *testing.T) {
	registry := newFakeRegistry(persistedRegistration("01JXSBDEAD00000000000000AA", "MGIT-99", model.StateDead))
	svc, mgr, events := newRehydrateService(t, registry)
	mgr.resolveInfo = &model.SandboxInfo{ID: "01JXSBDEAD00000000000000AA", State: model.StateRunning}

	report, err := svc.Rehydrate(context.Background())
	require.NoError(t, err)
	assert.Equal(t, []string{"MGIT-99"}, report.Recovered)

	st, err := svc.Status(context.Background(), "MGIT-99")
	require.NoError(t, err)
	assert.Equal(t, model.StateDead, st.State, "the backend's live VM process does not resurrect a dead guest")

	_, err = svc.Exec(context.Background(), "MGIT-99", model.ExecRequest{Command: []string{"true"}})
	require.ErrorIs(t, err, model.ErrGuestDead)
	assert.Zero(t, mgr.execs)
	assert.Contains(t, err.Error(), "mgit sandbox remove MGIT-99 --force")
	assert.NotContains(t, events.types(), model.EventKilled, "adopting a dead row is not a kill")

	require.NoError(t, svc.Remove(context.Background(), "MGIT-99", true))
	assert.Equal(t, 1, mgr.stops, "the process that outlived the guest is stopped on remove")
}
