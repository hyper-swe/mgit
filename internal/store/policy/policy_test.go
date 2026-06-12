// Package policy tests verify the SEC-02 host-only policy store per
// MGIT-11.3.4 acceptance criteria. Refs: FR-17.13, FR-17.6
package policy

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/hyper-swe/mgit/internal/model"
)

func fixedClock() func() time.Time {
	fixed := time.Date(2026, 6, 12, 12, 0, 0, 0, time.UTC)
	return func() time.Time { return fixed }
}

// recordingSink captures policy-change events for assertions.
type recordingSink struct {
	details []string
}

func (r *recordingSink) RecordPolicyChange(_ context.Context, detail string) error {
	r.details = append(r.details, detail)
	return nil
}

// newTestStore returns a policy store rooted in a temp host dir, plus
// a worktree dir that must never be consulted.
func newTestStore(t *testing.T) (*Store, *recordingSink, string, string) {
	t.Helper()
	hostRoot := filepath.Join(t.TempDir(), "host", "repo-abc123")
	worktree := t.TempDir()
	sink := &recordingSink{}
	store, err := NewStore(hostRoot, fixedClock(), sink)
	require.NoError(t, err)
	return store, sink, hostRoot, worktree
}

func writePolicyFile(t *testing.T, dir string, p model.SandboxPolicy) string {
	t.Helper()
	require.NoError(t, os.MkdirAll(dir, 0o700))
	data, err := json.Marshal(p)
	require.NoError(t, err)
	path := filepath.Join(dir, "policy.json")
	require.NoError(t, os.WriteFile(path, data, 0o600))
	return path
}

// TestPolicy_LoadedFromHostRoot_NotWorktree verifies SEC-02: policy
// reads target the host root; a worktree-resident policy file carrying
// weaker settings is never consulted. Refs: FR-17.13
func TestPolicy_LoadedFromHostRoot_NotWorktree(t *testing.T) {
	store, _, hostRoot, worktree := newTestStore(t)
	ctx := context.Background()

	hostPolicy := model.DefaultSandboxPolicy()
	hostPolicy.Network.Allowlist = []string{"proxy.golang.org"}
	require.NoError(t, store.Save(ctx, hostPolicy))

	// A hostile worktree policy tries to weaken enforcement (SEC-02).
	weak := model.DefaultSandboxPolicy()
	weak.RequireSandbox = false
	weak.Network = model.NetworkPolicy{Mode: model.NetworkModeOpen}
	writePolicyFile(t, filepath.Join(worktree, ".mgit", "sandbox"), weak)

	got, err := store.Load(ctx)
	require.NoError(t, err)
	assert.True(t, got.RequireSandbox, "host policy governs, not the worktree file")
	assert.Equal(t, model.NetworkModeAllowlist, got.Network.Mode)
	assert.Equal(t, []string{"proxy.golang.org"}, got.Network.Allowlist)

	t.Run("policy_file_lives_under_host_root", func(t *testing.T) {
		assert.FileExists(t, filepath.Join(hostRoot, "policy.json"))
	})

	t.Run("file_permissions_are_owner_only", func(t *testing.T) {
		info, err := os.Stat(filepath.Join(hostRoot, "policy.json"))
		require.NoError(t, err)
		assert.Equal(t, os.FileMode(0o600), info.Mode().Perm())
	})
}

// TestPolicy_WorktreeFileIgnoredWithoutAdoption verifies the adoption
// flow: repo-suggested defaults take effect only after explicit
// host-side adoption, which itself emits a policy event. Refs: FR-17.13
func TestPolicy_WorktreeFileIgnoredWithoutAdoption(t *testing.T) {
	store, sink, _, worktree := newTestStore(t)
	ctx := context.Background()

	suggested := model.DefaultSandboxPolicy()
	suggested.Network.Allowlist = []string{"registry.npmjs.org"}
	suggestedPath := writePolicyFile(t, filepath.Join(worktree, ".mgit", "sandbox"), suggested)

	got, err := store.Load(ctx)
	require.NoError(t, err)
	assert.Empty(t, got.Network.Allowlist,
		"un-adopted worktree suggestions must not take effect")

	require.NoError(t, store.AdoptSuggested(ctx, suggestedPath))

	got, err = store.Load(ctx)
	require.NoError(t, err)
	assert.Equal(t, []string{"registry.npmjs.org"}, got.Network.Allowlist,
		"adoption copies the suggestion into the host root")
	assert.NotEmpty(t, sink.details, "adoption is an effective-policy change and must be audited")

	t.Run("invalid_suggestion_rejected", func(t *testing.T) {
		bad := model.SandboxPolicy{Network: model.NetworkPolicy{Mode: "bridge"}}
		badPath := writePolicyFile(t, filepath.Join(worktree, "bad"), bad)
		assert.Error(t, store.AdoptSuggested(ctx, badPath))
	})
}

// TestPolicy_ChangeEmitsAuditEvent verifies every effective-policy
// change is recorded. Refs: FR-17.13, FR-17.6
func TestPolicy_ChangeEmitsAuditEvent(t *testing.T) {
	store, sink, _, _ := newTestStore(t)
	ctx := context.Background()

	p := model.DefaultSandboxPolicy()
	require.NoError(t, store.Save(ctx, p))
	require.Len(t, sink.details, 1, "save must emit one policy event")
	assert.Contains(t, sink.details[0], `"require_sandbox":true`)

	p.RequireSandbox = false
	require.NoError(t, store.Save(ctx, p))
	require.Len(t, sink.details, 2)
	assert.Contains(t, sink.details[1], `"require_sandbox":false`,
		"disabling require_sandbox is itself audited (FR-17.6)")
}

// TestPolicy_RequireSandboxDefaultsTrue verifies the FR-17.6 default:
// with no host policy file, require_sandbox is true; a policy file
// that omits the field keeps the safe default. Refs: FR-17.6
func TestPolicy_RequireSandboxDefaultsTrue(t *testing.T) {
	store, _, hostRoot, _ := newTestStore(t)
	ctx := context.Background()

	got, err := store.Load(ctx)
	require.NoError(t, err)
	assert.True(t, got.RequireSandbox, "absent policy file => safe defaults")
	assert.Equal(t, model.NetworkModeAllowlist, got.Network.Mode,
		"default network mode is allowlist, never open")
	assert.NotEmpty(t, got.SensitivePaths, "host-trusted path list has safe defaults")

	t.Run("omitted_field_keeps_safe_default", func(t *testing.T) {
		require.NoError(t, os.MkdirAll(hostRoot, 0o700))
		require.NoError(t, os.WriteFile(filepath.Join(hostRoot, "policy.json"),
			[]byte(`{"network":{"mode":"none"}}`), 0o600))

		got, err := store.Load(ctx)
		require.NoError(t, err)
		assert.True(t, got.RequireSandbox,
			"a policy file omitting require_sandbox must not weaken it")
		assert.Equal(t, model.NetworkModeNone, got.Network.Mode)
	})

	t.Run("corrupt_policy_file_fails_closed", func(t *testing.T) {
		require.NoError(t, os.WriteFile(filepath.Join(hostRoot, "policy.json"),
			[]byte(`{not json`), 0o600))
		_, err := store.Load(ctx)
		assert.Error(t, err, "a corrupt policy file must fail, not fall back silently")
	})

	t.Run("invalid_policy_file_fails_closed", func(t *testing.T) {
		require.NoError(t, os.WriteFile(filepath.Join(hostRoot, "policy.json"),
			[]byte(`{"network":{"mode":"bridge"}}`), 0o600))
		_, err := store.Load(ctx)
		assert.Error(t, err, "an invalid policy must fail, not fall back silently")
	})

	t.Run("unreadable_policy_file_fails_closed", func(t *testing.T) {
		require.NoError(t, os.Remove(filepath.Join(hostRoot, "policy.json")))
		require.NoError(t, os.Mkdir(filepath.Join(hostRoot, "policy.json"), 0o700))
		_, err := store.Load(ctx)
		assert.Error(t, err, "a read error must surface, not silently default")
	})
}

// failingSink simulates an audit sink outage.
type failingSink struct{}

func (failingSink) RecordPolicyChange(context.Context, string) error {
	return assert.AnError
}

// TestPolicy_ErrorPaths covers constructor guards and save/adopt
// failures. Refs: FR-17.13
func TestPolicy_ErrorPaths(t *testing.T) {
	ctx := context.Background()

	t.Run("constructor_guards", func(t *testing.T) {
		_, err := NewStore("", fixedClock(), &recordingSink{})
		assert.Error(t, err, "empty host root rejected")
		_, err = NewStore(t.TempDir(), nil, &recordingSink{})
		assert.Error(t, err, "nil clock rejected")
		_, err = NewStore(t.TempDir(), fixedClock(), nil)
		assert.Error(t, err, "nil event recorder rejected")
	})

	t.Run("save_invalid_policy_rejected", func(t *testing.T) {
		store, _, _, _ := newTestStore(t)
		bad := model.DefaultSandboxPolicy()
		bad.Network.Mode = "bridge"
		assert.Error(t, store.Save(ctx, bad))
	})

	t.Run("save_surfaces_audit_sink_failure", func(t *testing.T) {
		store, err := NewStore(filepath.Join(t.TempDir(), "root"), fixedClock(), failingSink{})
		require.NoError(t, err)
		assert.Error(t, store.Save(ctx, model.DefaultSandboxPolicy()),
			"an unrecordable policy change must not be applied silently")
	})

	t.Run("save_unwritable_root_rejected", func(t *testing.T) {
		blocker := filepath.Join(t.TempDir(), "blocker")
		require.NoError(t, os.WriteFile(blocker, []byte("x"), 0o600))
		store, err := NewStore(filepath.Join(blocker, "root"), fixedClock(), &recordingSink{})
		require.NoError(t, err)
		assert.Error(t, store.Save(ctx, model.DefaultSandboxPolicy()),
			"an unwritable host root must surface, not be swallowed")
	})

	t.Run("adopt_missing_suggestion_rejected", func(t *testing.T) {
		store, _, _, _ := newTestStore(t)
		assert.Error(t, store.AdoptSuggested(ctx, filepath.Join(t.TempDir(), "absent.json")))
	})

	t.Run("adopt_corrupt_suggestion_rejected", func(t *testing.T) {
		store, _, _, worktree := newTestStore(t)
		path := filepath.Join(worktree, "corrupt.json")
		require.NoError(t, os.WriteFile(path, []byte(`{not json`), 0o600))
		assert.Error(t, store.AdoptSuggested(ctx, path))
	})
}
