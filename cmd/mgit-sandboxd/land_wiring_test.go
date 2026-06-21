package main

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/hyper-swe/mgit/internal/model"
	"github.com/hyper-swe/mgit/internal/sandboxd"
	gitstore "github.com/hyper-swe/mgit/internal/store/git"
	"github.com/hyper-swe/mgit/internal/store/index"
	"github.com/hyper-swe/mgit/internal/store/policy"
)

type stubResolver struct{}

func (stubResolver) Status(context.Context, string) (*model.SandboxInfo, error) {
	return &model.SandboxInfo{ID: "sbx-1"}, nil
}

// TestBuildLandService_WiresAndBootstrapsAttestKey verifies the land path
// wires against a real host repo and self-generates the host attestation key
// on first use, returning a usable lander. Refs: MGIT-11.10.10, FR-17.38
func TestBuildLandService_WiresAndBootstrapsAttestKey(t *testing.T) {
	clock := func() time.Time { return time.Unix(0, 0).UTC() }
	repoRoot := t.TempDir()
	repo, err := gitstore.Init(repoRoot, clock)
	require.NoError(t, err)
	require.NoError(t, repo.Close())

	hostRoot := filepath.Join(repoRoot, ".mgit", "sandbox")
	require.NoError(t, os.MkdirAll(hostRoot, 0o700))

	events, err := index.New(filepath.Join(hostRoot, sandboxIndexDB), clock)
	require.NoError(t, err)
	t.Cleanup(func() { _ = events.Close() })
	policyStore, err := policy.NewStore(hostRoot, clock, slogPolicyRecorder{logger: testLogger()})
	require.NoError(t, err)

	binder := sandboxd.NewPeerBinder(testLogger())
	lander, closeLand, err := buildLandService(hostRoot, repoRoot, t.TempDir(), stubResolver{},
		events, policyStore, binder, clock, testLogger())
	require.NoError(t, err)
	require.NotNil(t, lander)
	t.Cleanup(func() { _ = closeLand() })

	// The attestation key was generated host-side (0600, under trust).
	keyPath := filepath.Join(hostRoot, "trust", "attestation-signing.key")
	fi, err := os.Stat(keyPath)
	require.NoError(t, err, "the host attestation key is generated on first land wiring")
	assert.Equal(t, os.FileMode(0o600), fi.Mode().Perm(), "the private key is owner-only")
}

// TestBuildLandService_BadRepo_Error verifies wiring fails closed when the
// host repo is absent (no .mgit), so the daemon logs and serves land "not
// served" rather than half-wired.
func TestBuildLandService_BadRepo_Error(t *testing.T) {
	clock := func() time.Time { return time.Unix(0, 0).UTC() }
	// hostRoot points under a dir with no .mgit repo at the derived root.
	hostRoot := filepath.Join(t.TempDir(), ".mgit", "sandbox")
	require.NoError(t, os.MkdirAll(hostRoot, 0o700))
	events, err := index.New(filepath.Join(hostRoot, sandboxIndexDB), clock)
	require.NoError(t, err)
	t.Cleanup(func() { _ = events.Close() })
	policyStore, err := policy.NewStore(hostRoot, clock, slogPolicyRecorder{logger: testLogger()})
	require.NoError(t, err)

	// Empty repo root exercises the host-root fallback (which derives a root
	// with no .mgit repo).
	_, _, err = buildLandService(hostRoot, "", t.TempDir(), stubResolver{}, events, policyStore,
		sandboxd.NewPeerBinder(testLogger()), clock, testLogger())
	assert.Error(t, err)
}
