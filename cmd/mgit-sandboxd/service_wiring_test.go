package main

import (
	"bytes"
	"context"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/hyper-swe/mgit/internal/model"
	"github.com/hyper-swe/mgit/internal/sandboxd"
	"github.com/hyper-swe/mgit/internal/service"
)

// The concrete service the daemon is wired with MUST satisfy the daemon's
// dispatch contract — the one assertion that keeps cmd wiring honest.
var _ sandboxd.SandboxDispatcher = (*service.SandboxService)(nil)

// nopManager is a minimal SandboxManager for wiring tests (no real VMM).
type nopManager struct{}

func (nopManager) Launch(context.Context, model.SandboxLaunchOptions) (*model.SandboxInfo, error) {
	return &model.SandboxInfo{}, nil
}
func (nopManager) List(context.Context) ([]model.SandboxInfo, error) { return nil, nil }
func (nopManager) Stop(context.Context, string, bool) error          { return nil }
func (nopManager) Remove(context.Context, string, bool) error        { return nil }
func (nopManager) Resolve(context.Context, string) (*model.SandboxInfo, error) {
	return nil, nil
}
func (nopManager) Exec(context.Context, string, model.ExecRequest) (*model.ExecResult, error) {
	return &model.ExecResult{}, nil
}

func testLogger() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

// TestBuildSandboxService_WiresServiceAndCloser verifies the production
// wiring builds a usable service backed by a real audit index under the
// host root, and returns a closer that releases it cleanly.
func TestBuildSandboxService_WiresServiceAndCloser(t *testing.T) {
	clock := func() time.Time { return time.Unix(0, 0).UTC() }
	hostRoot := t.TempDir()
	policyStore := newPolicyStore(hostRoot, clock, testLogger())
	require.NotNil(t, policyStore)
	svc, events, closeAudit, err := buildSandboxService(nopManager{}, hostRoot, policyStore, clock)
	require.NoError(t, err)
	require.NotNil(t, svc)
	require.NotNil(t, events)
	require.NotNil(t, closeAudit)

	// The service is live: a register records into the real audit index.
	_, err = svc.Register(context.Background(), model.SandboxLaunchOptions{
		TaskID: "MGIT-1", WorktreePath: "/work/a",
		ImageRef: "img@sha256:" + repeat64('a'),
		Network:  model.NetworkPolicy{Mode: model.NetworkModeNone},
	})
	require.NoError(t, err)
	assert.NoError(t, closeAudit(), "the audit store closes cleanly")
}

// TestNewIDGen_MonotonicUnique verifies generated IDs are unique and
// sortable in creation order (monotonic ULIDs).
func TestNewIDGen_MonotonicUnique(t *testing.T) {
	clock := func() time.Time { return time.Unix(0, 0).UTC() }
	gen := newIDGen(clock)
	const n = 200
	seen := make(map[string]struct{}, n)
	ids := make([]string, 0, n)
	for i := 0; i < n; i++ {
		id, err := gen()
		require.NoError(t, err)
		_, dup := seen[id]
		require.False(t, dup, "ids must be unique")
		seen[id] = struct{}{}
		ids = append(ids, id)
	}
	assert.True(t, sort.StringsAreSorted(ids), "monotonic ids sort in creation order even at a fixed clock")
}

// TestBuildSandboxService_BadHostRoot verifies a host root that cannot
// host the audit index surfaces a wiring error (the daemon then exits 2
// rather than serving without an audit trail).
func TestBuildSandboxService_BadHostRoot(t *testing.T) {
	// A regular file masquerading as the host root: the audit index cannot
	// be created beneath it.
	f := filepath.Join(t.TempDir(), "not-a-dir")
	require.NoError(t, os.WriteFile(f, []byte("x"), 0o600))
	clock := func() time.Time { return time.Unix(0, 0).UTC() }
	_, _, _, err := buildSandboxService(nopManager{}, f, newPolicyStore(f, clock, testLogger()), clock)
	require.Error(t, err)

	t.Run("nil_policy_store_is_refused", func(t *testing.T) {
		// A serving daemon without a policy reader would enforce nothing:
		// no require_sandbox, no per-sandbox maxima, no network posture.
		// Refs: SEC-02, FR-17.13, MGIT-98
		_, _, _, err := buildSandboxService(nopManager{}, t.TempDir(), nil, clock)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "policy store")
	})
}

// TestSlogPolicyRecorder_LogsChange covers the policy-change recorder the
// service requires (the daemon only reads policy, so this rarely fires).
func TestSlogPolicyRecorder_LogsChange(t *testing.T) {
	var buf bytes.Buffer
	rec := slogPolicyRecorder{logger: slog.New(slog.NewTextHandler(&buf, nil))}
	require.NoError(t, rec.RecordPolicyChange(context.Background(), "require_sandbox=false"))
	assert.Contains(t, buf.String(), "policy_change")
}

func repeat64(b byte) string {
	out := make([]byte, 64)
	for i := range out {
		out[i] = b
	}
	return string(out)
}

// TestBuildSandboxService_CreatesTheHostRootItOwns covers the very first
// `mgit sandbox …` command in a fresh repo.
//
// .mgit/sandbox is the daemon's own directory — its audit index, policy and
// trust root all live there — but nothing created it before the daemon tried
// to open a database inside it. A user who ran any sandbox command before
// `mgit sandbox image init` got a daemon that died with
//
//	open sandbox audit index: … unable to open database file: out of memory (14)
//
// which the CLI then reported as "not dialable after spawn". Two unrelated
// pieces of nonsense for one missing mkdir. Refs: MGIT-61.15, FR-17.13
func TestBuildSandboxService_CreatesTheHostRootItOwns(t *testing.T) {
	hostRoot := filepath.Join(t.TempDir(), "repo", ".mgit", "sandbox")
	clock := func() time.Time { return time.Unix(0, 0).UTC() }

	_, _, closeAudit, err := buildSandboxService(nopManager{}, hostRoot,
		newPolicyStore(hostRoot, clock, testLogger()), clock)

	require.NoError(t, err, "the daemon must stand up the directory it owns")
	t.Cleanup(func() { _ = closeAudit() })
	require.DirExists(t, hostRoot)
	require.FileExists(t, filepath.Join(hostRoot, sandboxIndexDB))
}
