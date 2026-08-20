package service

import (
	"context"
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/hyper-swe/mgit/internal/model"
)

// declaringManager is a backend that declares which network modes it can
// actually ENFORCE, as the vzf and container backends do.
type declaringManager struct {
	*fakeSandboxManager
	refuse map[string]error
}

func (m *declaringManager) SupportsNetworkMode(mode string) error {
	return m.refuse[mode]
}

// THE DEFECT. A backend that cannot enforce an allowlist refuses it when the
// VM is CREATED — vzf's refuseUnenforceableNetwork is the first statement of
// CreateVM. But provisioning is lazy (FR-17.9/17.10), so `sandbox launch
// --network allowlist` on such a backend REGISTERED HAPPILY and reported
// "created". The operator has configured containment that can never exist and
// is not told until first use, which may be many minutes later.
//
// Nothing runs uncontained — the boot fails closed — so this is an honesty
// defect rather than a containment breach. But the moment of diagnosis is the
// moment of configuration, and moving it there costs nothing. Refs: MGIT-111, SEC-04
func TestRegister_BackendThatCannotEnforceTheMode_IsRefusedAtRegistration(t *testing.T) {
	unenforceable := errors.New("allowlist mode is not enforceable on this backend")
	tests := []struct {
		name    string
		mode    string
		wantErr bool
	}{
		{name: "allowlist_the_backend_cannot_enforce_is_refused", mode: model.NetworkModeAllowlist, wantErr: true},
		{name: "none_is_accepted", mode: model.NetworkModeNone},
		{name: "open_is_accepted", mode: model.NetworkModeOpen},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			mgr := &declaringManager{
				fakeSandboxManager: &fakeSandboxManager{},
				refuse:             map[string]error{model.NetworkModeAllowlist: unenforceable},
			}
			svc := newSvc(t, mgr, &fakeEventAppender{})
			opts := regOpts("MGIT-111", t.TempDir())
			opts.Network = model.NetworkPolicy{Mode: tt.mode}
			if tt.mode == model.NetworkModeAllowlist {
				opts.Network.Allowlist = []string{"example.com"}
			}

			info, err := svc.Register(context.Background(), opts)
			if tt.wantErr {
				require.Error(t, err, "registration must refuse a mode this backend cannot enforce")
				assert.ErrorIs(t, err, unenforceable, "the backend's own reason must reach the operator")
				assert.Nil(t, info, "nothing may be registered when the mode cannot be enforced")
				return
			}
			require.NoError(t, err)
			require.NotNil(t, info)
		})
	}
}

// A backend that declares nothing keeps today's behavior exactly: the check is
// an optional extension, not a new requirement every backend must satisfy.
// Refs: MGIT-111
func TestRegister_BackendThatDeclaresNothing_IsUnaffected(t *testing.T) {
	svc := newSvc(t, &fakeSandboxManager{}, &fakeEventAppender{})
	opts := regOpts("MGIT-111", t.TempDir())
	opts.Network = model.NetworkPolicy{Mode: model.NetworkModeAllowlist, Allowlist: []string{"example.com"}}

	info, err := svc.Register(context.Background(), opts)
	require.NoError(t, err)
	require.NotNil(t, info)
}

// The refusal must land BEFORE anything is recorded, so a refused launch
// leaves no registration and no worktree binding behind for the operator to
// clean up. Refs: MGIT-111
func TestRegister_RefusedMode_LeavesNoRegistrationBehind(t *testing.T) {
	mgr := &declaringManager{
		fakeSandboxManager: &fakeSandboxManager{},
		refuse:             map[string]error{model.NetworkModeAllowlist: errors.New("cannot enforce")},
	}
	svc := newSvc(t, mgr, &fakeEventAppender{})
	opts := regOpts("MGIT-111", t.TempDir())
	opts.Network = model.NetworkPolicy{Mode: model.NetworkModeAllowlist, Allowlist: []string{"example.com"}}

	_, err := svc.Register(context.Background(), opts)
	require.Error(t, err)

	listed, lerr := svc.List(context.Background())
	require.NoError(t, lerr)
	assert.Empty(t, listed, "a refused registration must not leave a sandbox behind")

	// And the task is still free to be registered with a mode that works.
	opts.Network = model.NetworkPolicy{Mode: model.NetworkModeNone}
	info, err := svc.Register(context.Background(), opts)
	require.NoError(t, err, "the task binding must not have been consumed by the refusal")
	require.NotNil(t, info)
}
