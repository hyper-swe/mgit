// Package sandboxd tests verify the global resource ceiling per
// MGIT-11.4.3 acceptance criteria (SEC-09). Refs: FR-17.26
package sandboxd

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/hyper-swe/mgit/internal/model"
)

// launchingManager is a fake backend whose Launch always succeeds and
// registers the sandbox (memory included) for List-based accounting.
type launchingManager struct {
	fakeManager
	next int
}

func (m *launchingManager) Launch(_ context.Context, opts model.SandboxLaunchOptions) (*model.SandboxInfo, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.next++
	info := model.SandboxInfo{
		ID: fmt.Sprintf("01JXCEIL%017d", m.next), TaskID: opts.TaskID,
		State: model.StateRunning, MemoryMB: opts.MemoryMB,
	}
	m.sandboxes[info.ID] = info
	return &info, nil
}

func launchOpts(task string, memoryMB int) model.SandboxLaunchOptions {
	return model.SandboxLaunchOptions{
		TaskID:       task,
		WorktreePath: "/w",
		ImageRef:     "go-node@sha256:" + strings.Repeat("a", 64),
		Network:      model.NetworkPolicy{Mode: model.NetworkModeAllowlist},
		MemoryMB:     memoryMB,
	}
}

// TestCeiling_ExceedConcurrency_QueuesOrRejects verifies launches past
// the concurrency cap fail fast with the typed error and recover after
// a removal. Refs: FR-17.26
func TestCeiling_ExceedConcurrency_QueuesOrRejects(t *testing.T) {
	inner := &launchingManager{fakeManager: *newFakeManager()}
	mgr := NewCeilingManager(inner, 2, 0, 0)
	ctx := context.Background()

	first, err := mgr.Launch(ctx, launchOpts("MGIT-1.1", 512))
	require.NoError(t, err)
	_, err = mgr.Launch(ctx, launchOpts("MGIT-1.2", 512))
	require.NoError(t, err)

	_, err = mgr.Launch(ctx, launchOpts("MGIT-1.3", 512))
	require.ErrorIs(t, err, model.ErrSandboxCeilingExceeded,
		"the third concurrent launch must fail fast with the typed error")

	require.NoError(t, mgr.Remove(ctx, first.ID, true))
	_, err = mgr.Launch(ctx, launchOpts("MGIT-1.3", 512))
	assert.NoError(t, err, "capacity freed by removal is reusable")
}

// TestCeiling_TotalMemoryBounded verifies the aggregate memory ceiling
// (SEC-09): per-VM caps alone cannot protect the host. Refs: FR-17.26
func TestCeiling_TotalMemoryBounded(t *testing.T) {
	inner := &launchingManager{fakeManager: *newFakeManager()}
	mgr := NewCeilingManager(inner, 0, 4096, 0)
	ctx := context.Background()

	_, err := mgr.Launch(ctx, launchOpts("MGIT-1.1", 2048))
	require.NoError(t, err)
	second, err := mgr.Launch(ctx, launchOpts("MGIT-1.2", 2048))
	require.NoError(t, err)

	_, err = mgr.Launch(ctx, launchOpts("MGIT-1.3", 1))
	require.ErrorIs(t, err, model.ErrSandboxCeilingExceeded,
		"even a tiny launch past the memory ceiling must be refused")

	require.NoError(t, mgr.Remove(ctx, second.ID, true))
	_, err = mgr.Launch(ctx, launchOpts("MGIT-1.3", 2048))
	assert.NoError(t, err, "memory freed by removal is reusable")

	t.Run("unresolved_memory_accounted_at_default", func(t *testing.T) {
		inner := &launchingManager{fakeManager: *newFakeManager()}
		mgr := NewCeilingManager(inner, 0, 2048, 0)
		_, err := mgr.Launch(ctx, launchOpts("MGIT-2.1", 0))
		require.NoError(t, err, "one default-sized launch fits")
		_, err = mgr.Launch(ctx, launchOpts("MGIT-2.2", 0))
		assert.ErrorIs(t, err, model.ErrSandboxCeilingExceeded,
			"a zero memory request is accounted at the NFR-17.5 default, never as free")
	})
}

// TestCeiling_Configurable verifies caps are configuration, with zero
// meaning unlimited for that dimension. Refs: FR-17.26
func TestCeiling_Configurable(t *testing.T) {
	ctx := context.Background()

	t.Run("zero_caps_unlimited", func(t *testing.T) {
		inner := &launchingManager{fakeManager: *newFakeManager()}
		mgr := NewCeilingManager(inner, 0, 0, 0)
		for i := 0; i < 20; i++ {
			_, err := mgr.Launch(ctx, launchOpts(fmt.Sprintf("MGIT-3.%d", i+1), 1024))
			require.NoError(t, err)
		}
	})

	t.Run("cap_of_one", func(t *testing.T) {
		inner := &launchingManager{fakeManager: *newFakeManager()}
		mgr := NewCeilingManager(inner, 1, 0, 0)
		_, err := mgr.Launch(ctx, launchOpts("MGIT-4.1", 512))
		require.NoError(t, err)
		_, err = mgr.Launch(ctx, launchOpts("MGIT-4.2", 512))
		assert.ErrorIs(t, err, model.ErrSandboxCeilingExceeded)
	})

	t.Run("concurrent_launch_race_respects_cap", func(t *testing.T) {
		inner := &launchingManager{fakeManager: *newFakeManager()}
		mgr := NewCeilingManager(inner, 5, 0, 0)

		var wg sync.WaitGroup
		var mu sync.Mutex
		succeeded := 0
		for i := 0; i < 20; i++ {
			wg.Add(1)
			go func(n int) {
				defer wg.Done()
				if _, err := mgr.Launch(ctx, launchOpts(fmt.Sprintf("MGIT-5.%d", n+1), 64)); err == nil {
					mu.Lock()
					succeeded++
					mu.Unlock()
				}
			}(i)
		}
		wg.Wait()

		// The security property is NEVER overshooting (SEC-09). A racing
		// admission may transiently under-admit (a just-launched sandbox
		// is briefly counted as both reserved and listed) — the safe
		// direction; capacity is reachable once the race settles.
		assert.LessOrEqual(t, succeeded, 5, "racing launches must never overshoot the cap")
		for succeeded < 5 {
			_, err := mgr.Launch(ctx, launchOpts(fmt.Sprintf("MGIT-5.fill%d", succeeded), 64))
			require.NoError(t, err, "freed transient reservations must admit up to the cap")
			succeeded++
		}
		_, err := mgr.Launch(ctx, launchOpts("MGIT-5.overflow", 64))
		assert.ErrorIs(t, err, model.ErrSandboxCeilingExceeded,
			"the cap holds exactly once filled")
	})

	t.Run("destroyed_sandboxes_do_not_count", func(t *testing.T) {
		inner := &launchingManager{fakeManager: *newFakeManager()}
		inner.sandboxes["01JXDEAD"] = model.SandboxInfo{
			ID: "01JXDEAD", TaskID: "MGIT-7.0", State: model.StateDestroyed, MemoryMB: 999999,
		}
		mgr := NewCeilingManager(inner, 1, 1024, 0)
		_, err := mgr.Launch(ctx, launchOpts("MGIT-7.1", 512))
		assert.NoError(t, err, "destroyed sandboxes hold no capacity")
	})

	t.Run("admission_surfaces_list_failure", func(t *testing.T) {
		inner := &launchingManager{fakeManager: *newFakeManager()}
		inner.failList = true
		mgr := NewCeilingManager(inner, 1, 0, 0)
		_, err := mgr.Launch(ctx, launchOpts("MGIT-8.1", 512))
		assert.Error(t, err, "an unverifiable ceiling fails closed")
	})

	t.Run("delegated_operations_pass_through", func(t *testing.T) {
		inner := &launchingManager{fakeManager: *newFakeManager()}
		mgr := NewCeilingManager(inner, 1, 0, 0)
		info, err := mgr.Launch(ctx, launchOpts("MGIT-6.1", 64))
		require.NoError(t, err)

		listed, err := mgr.List(ctx)
		require.NoError(t, err)
		assert.Len(t, listed, 1)

		resolved, err := mgr.Resolve(ctx, info.ID)
		require.NoError(t, err)
		assert.Equal(t, info.ID, resolved.ID)

		_, err = mgr.Exec(ctx, info.ID, model.ExecRequest{Command: []string{"true"}})
		require.NoError(t, err)
		require.NoError(t, mgr.Stop(ctx, info.ID, false))
	})
}

// TestCeiling_AccountedDefault_UsesTheInjectedPolicyDefault verifies an
// undeclared launch is accounted at the DEFAULT THE CALLER INJECTED — the
// running host policy's memory_mb — not at the package fallback.
//
// Before MGIT-98 the daemon passed 0 here, so accounting always used the 2048
// MB fallback regardless of what host policy actually gave an undeclared
// sandbox. On a policy whose default is smaller the ceiling refused launches
// that fit; on a larger one it admitted a fleet that did not. Either way the
// ceiling was counting something other than what the host was handing out.
// Refs: FR-17.26, MGIT-98
func TestCeiling_AccountedDefault_UsesTheInjectedPolicyDefault(t *testing.T) {
	ctx := context.Background()

	t.Run("smaller_policy_default_admits_more_sandboxes", func(t *testing.T) {
		inner := &launchingManager{fakeManager: *newFakeManager()}
		// A 4096 MB ceiling with a 1024 MB policy default: four undeclared
		// launches fit. With the 2048 MB package fallback only two would.
		mgr := NewCeilingManager(inner, 0, 4096, 1024)
		for i := range 4 {
			_, err := mgr.Launch(ctx, launchOpts(fmt.Sprintf("MGIT-98.%d", i+1), 0))
			require.NoError(t, err, "launch %d is accounted at the injected 1024 MB default", i+1)
		}
		_, err := mgr.Launch(ctx, launchOpts("MGIT-98.5", 0))
		assert.ErrorIs(t, err, model.ErrSandboxCeilingExceeded,
			"the fifth undeclared launch exceeds the ceiling at the injected default")
	})

	t.Run("larger_policy_default_refuses_sooner", func(t *testing.T) {
		inner := &launchingManager{fakeManager: *newFakeManager()}
		mgr := NewCeilingManager(inner, 0, 8192, 4096)
		_, err := mgr.Launch(ctx, launchOpts("MGIT-98.6", 0))
		require.NoError(t, err)
		_, err = mgr.Launch(ctx, launchOpts("MGIT-98.7", 0))
		require.NoError(t, err)
		_, err = mgr.Launch(ctx, launchOpts("MGIT-98.8", 0))
		assert.ErrorIs(t, err, model.ErrSandboxCeilingExceeded,
			"a 4096 MB policy default fills an 8192 MB ceiling after two undeclared launches")
	})

	t.Run("zero_still_selects_the_package_fallback", func(t *testing.T) {
		inner := &launchingManager{fakeManager: *newFakeManager()}
		mgr := NewCeilingManager(inner, 0, 2048, 0)
		_, err := mgr.Launch(ctx, launchOpts("MGIT-98.9", 0))
		require.NoError(t, err)
		_, err = mgr.Launch(ctx, launchOpts("MGIT-98.10", 0))
		assert.ErrorIs(t, err, model.ErrSandboxCeilingExceeded,
			"an uninjected default must never be accounted as free")
	})
}

// TestCeiling_RefusalsStayDistinguishableFromThePerSandboxBound is the pin the
// two refusals must not drift past.
//
// MGIT-95 introduced ErrSandboxResourceLimitExceeded ("this launch is too big
// — ask for less") and this decorator owns ErrSandboxCeilingExceeded ("the host
// is full — free capacity or wait"). They have different fixes, so they must
// never be reachable through one another's sentinel and must never borrow each
// other's remedy. MGIT-98 changed this refusal's INPUTS (the ceiling now comes
// from host policy), which is exactly when wording drifts.
// Refs: FR-17.26, R-H212, MGIT-95, MGIT-98
func TestCeiling_RefusalsStayDistinguishableFromThePerSandboxBound(t *testing.T) {
	ctx := context.Background()
	inner := &launchingManager{fakeManager: *newFakeManager()}
	mgr := NewCeilingManager(inner, 0, 4096, 2048)

	_, err := mgr.Launch(ctx, launchOpts("MGIT-98.11", 4096))
	require.NoError(t, err, "one launch that exactly fills the ceiling is legal")

	_, err = mgr.Launch(ctx, launchOpts("MGIT-98.12", 512))
	require.Error(t, err)

	assert.ErrorIs(t, err, model.ErrSandboxCeilingExceeded)
	assert.NotErrorIs(t, err, model.ErrSandboxResourceLimitExceeded,
		"a full fleet must never surface as an over-sized launch")

	msg := err.Error()
	assert.Contains(t, msg, "in use or admitted",
		"the message must keep saying ADMITTED: libkrun and firecracker allocate guest "+
			"memory lazily, so this ceiling counts admitted memory, not resident memory")
	assert.Contains(t, msg, "free capacity",
		"the fleet refusal's remedy is freeing capacity or waiting")
	assert.NotContains(t, msg, "per-sandbox maximum",
		"the fleet refusal must not borrow the per-sandbox refusal's language")
	assert.NotContains(t, msg, "--memory-mb",
		"pointing a full-host refusal at --memory-mb would tell the caller to shrink a "+
			"workload that is not the problem — the R-H212 defect in reverse")
}

// TestCeiling_LaunchLargerThanTheWholeCeiling_SaysTheHostIsTooSmall covers the
// small-host case MGIT-98 has to behave sensibly in.
//
// On a modest host, 50% of physical memory can be BELOW the per-sandbox default
// (2048 MB), so every launch — including one that declared nothing at all — is
// refused. "Free capacity or wait" is then actively misleading advice: there is
// no capacity to free and waiting never helps. The refusal stays
// ErrSandboxCeilingExceeded (it is still a statement about the host, not about
// the request), but it names the real remedy. Refs: FR-17.26, MGIT-98
func TestCeiling_LaunchLargerThanTheWholeCeiling_SaysTheHostIsTooSmall(t *testing.T) {
	ctx := context.Background()
	// A 1500 MB ceiling (50% of a ~3 GiB host) under a 2048 MB policy default:
	// an undeclared launch cannot fit on a completely idle host.
	inner := &launchingManager{fakeManager: *newFakeManager()}
	mgr := NewCeilingManager(inner, 0, 1500, 2048)

	_, err := mgr.Launch(ctx, launchOpts("MGIT-98.13", 0))

	require.Error(t, err, "an idle host too small for one default sandbox still refuses")
	assert.ErrorIs(t, err, model.ErrSandboxCeilingExceeded)
	assert.NotErrorIs(t, err, model.ErrSandboxResourceLimitExceeded)

	msg := err.Error()
	assert.Contains(t, msg, "idle host",
		"the operator must learn that waiting and freeing capacity cannot help")
	assert.Contains(t, msg, "max_total_memory_percent",
		"the message must name the host-policy knob that actually governs this ceiling")
	assert.NotContains(t, msg, "free capacity",
		"there is nothing to free on an idle host; that advice belongs to the fleet-full refusal")

	t.Run("still_refuses_when_the_fleet_is_busy", func(t *testing.T) {
		inner := &launchingManager{fakeManager: *newFakeManager()}
		mgr := NewCeilingManager(inner, 0, 1500, 512)
		_, err := mgr.Launch(ctx, launchOpts("MGIT-98.14", 512))
		require.NoError(t, err)
		_, err = mgr.Launch(ctx, launchOpts("MGIT-98.15", 4096))
		require.Error(t, err)
		assert.ErrorIs(t, err, model.ErrSandboxCeilingExceeded)
		assert.Contains(t, err.Error(), "idle host",
			"a request bigger than the whole ceiling is unfittable regardless of current usage")
	})
}
