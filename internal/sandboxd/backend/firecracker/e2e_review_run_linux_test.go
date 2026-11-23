//go:build linux

package firecracker

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/hyper-swe/mgit/internal/controlproto"
	"github.com/hyper-swe/mgit/internal/model"
	"github.com/hyper-swe/mgit/internal/reviewprep"
	"github.com/hyper-swe/mgit/internal/sandboxd"
	"github.com/hyper-swe/mgit/internal/sandboxd/attest"
	"github.com/hyper-swe/mgit/internal/sandboxd/reviewsvc"
)

// TestE2E_ReviewRun_ThroughTheRunService prepares and runs reviews on a real
// firecracker guest through the same service the daemon's run verb calls: the
// mapped program's output comes back, the lease cannot be run twice, a program
// that tries to write is refused by the launcher, an output over the limit is
// refused with the reviewer marked started, a reviewer past its time limit is
// refused with RUN_TIMEOUT, and every guest is gone afterwards.
// Refs: FEAT-3.347.1, MGIT-333
func TestE2E_ReviewRun_ThroughTheRunService(t *testing.T) {
	kernel, rootfs := requireE2EGuest(t)
	clock := func() time.Time { return time.Now().UTC() }
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
	binder := sandboxd.NewPeerBinder(logger)
	mgr, ref, _ := e2eBootGuest(t, kernel, rootfs, binder, clock, logger)
	ctx := context.Background()

	// The mapped entrypoint is the image's busybox; its digest is read inside a
	// guest of that image, as the operator would pin it.
	probe, err := mgr.Launch(ctx, model.SandboxLaunchOptions{
		TaskID: "REVIEW-DIGEST", WorktreePath: t.TempDir(), ImageRef: ref,
		Network: model.NetworkPolicy{Mode: model.NetworkModeNone}, CPUs: 1, MemoryMB: 256, NoGuestRepo: true,
	})
	require.NoError(t, err)
	sum, err := mgr.Exec(ctx, probe.ID, model.ExecRequest{Command: []string{"/bin/busybox", "sha256sum", "/bin/busybox"}, Timeout: 60 * time.Second})
	require.NoError(t, err)
	require.Zero(t, sum.ExitCode, string(sum.Stderr))
	digest := strings.Fields(string(sum.Stdout))[0]
	require.NoError(t, mgr.Remove(ctx, probe.ID, true))

	hostRoot := t.TempDir()
	require.NoError(t, attest.GenerateKey(ctx, hostRoot, reviewKeyAudit{}))
	svc, err := reviewsvc.New(hostRoot, clock)
	require.NoError(t, err)
	svc.WithRunner(reviewsvc.RunnerConfig{Manager: mgr, Backend: model.BackendKVM})

	install := func(id string, argv ...string) {
		raw, _ := json.Marshal(reviewprep.Mapping{
			Schema: reviewprep.MapSchema, ID: id, BindingSHA256: strings.Repeat("b", 64), ImageDigest: imageDigestOf(ref),
			GuestEntrypoint: "/bin/busybox", EntrypointSHA256: digest, Argv: argv, EgressPolicyID: "none",
		})
		require.NoError(t, os.MkdirAll(filepath.Join(hostRoot, "review", "maps"), 0o700))
		require.NoError(t, os.WriteFile(filepath.Join(hostRoot, "review", "maps", id+".json"), raw, 0o600))
	}
	prepare := func(id string) controlproto.ReviewRunArgs {
		bundle := t.TempDir()
		ev := []byte(`{"id":"` + id + `"}`)
		require.NoError(t, os.WriteFile(filepath.Join(bundle, "evidence.json"), ev, 0o600))
		h := sha256.Sum256(ev)
		nonce := "nonce-" + id + "-0123456789abcdef"
		raw, err := svc.PrepareReview(ctx, controlproto.ReviewPrepareArgs{
			Profile: reviewprep.Profile, Image: ref, MappingID: id, BindingDigest: strings.Repeat("b", 64),
			BundleDir: bundle, BundleDigest: hex.EncodeToString(h[:]), Nonce: nonce,
		})
		require.NoError(t, err)
		var att reviewprep.Attestation
		require.NoError(t, json.Unmarshal(raw, &att))
		return controlproto.ReviewRunArgs{
			Profile: reviewprep.Profile, Image: ref, MappingID: id, BindingDigest: strings.Repeat("b", 64),
			BundleDigest: hex.EncodeToString(h[:]), Nonce: nonce, Lease: att.Lease, MaxSeconds: 120,
		}
	}
	envelope := func(err error) *reviewprep.Error {
		var e *reviewprep.Error
		require.ErrorAs(t, err, &e)
		return e
	}

	// 1. The mapped program runs and its output comes back; the lease is single use.
	install("run-ok", "echo", "review-ok")
	a := prepare("run-ok")
	raw, err := svc.RunReview(ctx, a)
	require.NoError(t, err)
	var res controlproto.ReviewResult
	require.NoError(t, json.Unmarshal(raw, &res))
	assert.Equal(t, 0, res.ExitCode, string(res.Stderr))
	assert.Equal(t, "review-ok\n", string(res.Stdout))
	_, err = svc.RunReview(ctx, a)
	assert.Equal(t, reviewprep.CodeNonceReplayed, envelope(err).Code)

	// 2. A program that tries to write is refused by the launcher, and that is
	// the reviewer's own result, not a refusal of the run.
	install("run-write", "touch", "/tmp/review-write-attempt")
	raw, err = svc.RunReview(ctx, prepare("run-write"))
	require.NoError(t, err)
	require.NoError(t, json.Unmarshal(raw, &res))
	assert.NotZero(t, res.ExitCode)
	assert.Contains(t, string(res.Stderr), "Operation not permitted")

	// 3. Output over the limit is refused, the reviewer is marked started, and
	// none of the output is returned.
	install("run-big", "echo", "0123456789")
	b := prepare("run-big")
	b.MaxOutputBytes = 4
	raw, err = svc.RunReview(ctx, b)
	e := envelope(err)
	assert.Equal(t, reviewprep.CodeOutputLimit, e.Code)
	assert.True(t, e.ReviewerStarted)
	assert.Nil(t, raw)

	// 4. A reviewer that runs past its limit is refused with RUN_TIMEOUT, even
	// one that would have finished a few seconds later, and is marked started.
	install("run-slow", "sleep", "4")
	sl := prepare("run-slow")
	sl.MaxSeconds = 2
	raw, err = svc.RunReview(ctx, sl)
	e = envelope(err)
	assert.Equal(t, reviewprep.CodeRunTimeout, e.Code)
	assert.True(t, e.ReviewerStarted)
	assert.Nil(t, raw)

	// Every review guest was removed.
	infos, err := mgr.List(ctx)
	require.NoError(t, err)
	for _, in := range infos {
		assert.NotContains(t, in.TaskID, "REVIEW-", "a review guest outlived its run: %s", in.ID)
	}
}
