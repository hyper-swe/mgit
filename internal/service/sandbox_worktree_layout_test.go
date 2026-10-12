package service

import (
	"context"
	"errors"
	"fmt"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/hyper-swe/mgit/internal/model"
)

// layoutManager is a backend that checks a worktree's layout against its
// shared store, as the microVM and container backends do.
type layoutManager struct {
	*fakeSandboxManager
	refuse map[string]error
}

func (m *layoutManager) CheckWorktreeLayout(worktreePath string) error { return m.refuse[worktreePath] }

// REFUSED AT REGISTRATION, NOT AT FIRST BOOT (MGIT-222). A launch whose
// worktree holds the repository's store was registered and reported created,
// and the CLI then wrote agent scaffolding into the project's tracked
// CLAUDE.md; only the first boot refused. Registration now asks the backend
// the same layout question its boot asks, and a refusal registers nothing,
// records no event, and names the remedy. Refs: MGIT-222, SEC-03, MGIT-111
func TestRegister_AWorktreeThatHoldsTheStore_IsRefusedAtRegistration(t *testing.T) {
	root := t.TempDir()
	reachable := fmt.Errorf("%w: shared store %q is inside the mounted worktree %q",
		model.ErrSharedStoreReachable, root+"/.mgit", root)
	mgr := &layoutManager{fakeSandboxManager: &fakeSandboxManager{}, refuse: map[string]error{root: reachable}}
	events := &fakeEventAppender{}
	svc := newSvc(t, mgr, events)

	info, err := svc.Register(context.Background(), regOpts("MGIT-222", root))

	require.Error(t, err)
	assert.True(t, errors.Is(err, model.ErrSharedStoreReachable), "the backend's own reason reaches the operator: %v", err)
	for _, remedy := range []string{"mgit work", "mgit worktree add", "outside this repository"} {
		assert.Contains(t, err.Error(), remedy, "the refusal names the remedy")
	}
	assert.Nil(t, info, "nothing is registered")
	assert.Empty(t, events.events, "no created event is recorded for a sandbox that was refused")

	other := t.TempDir()
	_, err = svc.Register(context.Background(), regOpts("MGIT-223", other))
	require.NoError(t, err, "a worktree outside the store registers as before")
}
