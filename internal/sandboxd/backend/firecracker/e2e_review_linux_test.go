//go:build linux

package firecracker

import (
	"context"
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
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
	"github.com/hyper-swe/mgit/internal/reviewguest"
	"github.com/hyper-swe/mgit/internal/reviewprep"
	"github.com/hyper-swe/mgit/internal/sandboxd"
	"github.com/hyper-swe/mgit/internal/sandboxd/attest"
	"github.com/hyper-swe/mgit/internal/sandboxd/reviewsvc"
)

// TestE2E_ReviewGuest_LaunchTimeImage provisions the review guest on a real
// firecracker VM (the launch-time-image backend) and runs one pinned program
// through the launcher. The same program, run directly as the same user in the
// same guest, creates a file; through the launcher it cannot. That pair is what
// makes the denial a property of the launcher rather than of the image.
// Refs: FEAT-3.347.1, MGIT-333
func TestE2E_ReviewGuest_LaunchTimeImage(t *testing.T) {
	if os.Getuid() == 0 {
		t.Skip("the reviewer drops to the caller's unprivileged identity; run unprivileged")
	}
	kernel, rootfs := requireE2EGuest(t)
	clock := func() time.Time { return time.Now().UTC() }
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
	binder := sandboxd.NewPeerBinder(logger)
	mgr, ref, _ := e2eBootGuest(t, kernel, rootfs, binder, clock, logger)

	// The lease is sealed by the daemon's own review service, not by hand.
	old := reviewprep.WriteDenialEnforced
	reviewprep.WriteDenialEnforced = true
	t.Cleanup(func() { reviewprep.WriteDenialEnforced = old })
	hostRoot := t.TempDir()
	require.NoError(t, attest.GenerateKey(context.Background(), hostRoot, reviewKeyAudit{}))
	mapping := reviewprep.Mapping{
		Schema: reviewprep.MapSchema, ID: "live-v1", BindingSHA256: strings.Repeat("b", 64), ImageDigest: imageDigestOf(ref),
		GuestEntrypoint: "/bin/busybox", EntrypointSHA256: strings.Repeat("e", 64), Argv: []string{"true"}, EgressPolicyID: "none",
	}
	mraw, _ := json.Marshal(mapping)
	require.NoError(t, os.MkdirAll(filepath.Join(hostRoot, "review", "maps"), 0o700))
	require.NoError(t, os.WriteFile(filepath.Join(hostRoot, "review", "maps", "live-v1.json"), mraw, 0o600))
	bundle := t.TempDir()
	evidence := []byte(`{"k":"v"}`)
	require.NoError(t, os.WriteFile(filepath.Join(bundle, "evidence.json"), evidence, 0o600))
	sum := sha256.Sum256(evidence)
	args := controlproto.ReviewPrepareArgs{
		Profile: reviewprep.Profile, Image: ref, MappingID: "live-v1", BindingDigest: strings.Repeat("b", 64),
		BundleDir: bundle, BundleDigest: hex.EncodeToString(sum[:]), Nonce: "nonce-0123456789abcdef",
	}
	svc, err := reviewsvc.New(hostRoot, clock)
	require.NoError(t, err)
	rawAtt, err := svc.PrepareReview(context.Background(), args)
	require.NoError(t, err)
	var att reviewprep.Attestation
	require.NoError(t, json.Unmarshal(rawAtt, &att))
	priv, err := attest.LoadSigningKey(hostRoot)
	require.NoError(t, err)
	lease, err := reviewguest.OpenLease(hostRoot, att.Lease, reviewprep.Request{
		Profile: args.Profile, Image: args.Image, MappingID: args.MappingID, BindingDigest: args.BindingDigest,
		BundleDigest: args.BundleDigest, Nonce: args.Nonce,
	}, priv.Public().(ed25519.PublicKey), clock())
	require.NoError(t, err)
	leaseDir := lease.Dir()

	uid, gid := os.Getuid(), os.Getgid()
	ctx := context.Background()
	g, err := reviewguest.Provision(ctx, mgr, reviewguest.Spec{
		Backend: model.BackendKVM, Lease: lease, TaskID: "REV-LIVE", UID: uid, GID: gid,
	})
	require.NoError(t, err)
	t.Cleanup(func() { _ = g.Close(context.Background()) })

	same := &model.GuestIdentity{UID: uid, GID: gid}
	exec := func(cmd ...string) *model.ExecResult {
		t.Helper()
		res, err := mgr.Exec(ctx, g.ID, model.ExecRequest{Command: cmd, RunAs: same, Timeout: 60 * time.Second})
		require.NoError(t, err)
		return res
	}

	dsum := exec("/bin/busybox", "sha256sum", "/bin/busybox")
	require.Zero(t, dsum.ExitCode, string(dsum.Stderr))
	digest := strings.Fields(string(dsum.Stdout))[0]

	// Control: the same program, same user, no launcher.
	ctl := exec("/bin/busybox", "touch", filepath.Join(leaseDir, "control-marker"))
	require.Zero(t, ctl.ExitCode, "the control must be able to write, or the denial below proves nothing: %s", ctl.Stderr)

	// Through the launcher.
	t.Logf("guest kernel: %s", exec("/bin/busybox", "uname", "-a").Stdout)
	res, err := g.Run(ctx, reviewguest.Entry{SHA256: digest, Path: "/bin/busybox", Timeout: 90 * time.Second,
		Argv: []string{"touch", filepath.Join(leaseDir, "review-marker")}})
	if err != nil {
		// Say what the guest was doing, so a hang is a diagnosis and not a timeout.
		t.Logf("processes: %s", exec("/bin/busybox", "ps").Stdout)
		t.Logf("dmesg: %s", exec("/bin/busybox", "dmesg").Stdout)
	}
	require.NoError(t, err)
	assert.NotZero(t, res.ExitCode, "the reviewer's write must fail")

	ls := exec("/bin/busybox", "ls", leaseDir)
	require.Zero(t, ls.ExitCode, string(ls.Stderr))
	assert.Contains(t, string(ls.Stdout), "control-marker")
	assert.NotContains(t, string(ls.Stdout), "review-marker")
	assert.Contains(t, string(ls.Stdout), "evidence.json")

	// The host's directory is untouched either way: the image was built at launch.
	for _, n := range []string{"control-marker", "review-marker"} {
		_, statErr := os.Stat(filepath.Join(leaseDir, n))
		assert.True(t, errors.Is(statErr, os.ErrNotExist), "%s reached the host", n)
	}

	// A wrong digest is a launcher refusal that started nothing.
	_, err = g.Run(ctx, reviewguest.Entry{SHA256: strings.Repeat("0", 64), Path: "/bin/busybox", Argv: []string{"true"}})
	var re *reviewprep.Error
	require.ErrorAs(t, err, &re)
	assert.Equal(t, "HASH_MISMATCH", re.Code)
	assert.False(t, re.ReviewerStarted)
}

// reviewKeyAudit accepts the host attestation key's creation record.
type reviewKeyAudit struct{}

func (reviewKeyAudit) RecordKeyChange(context.Context, string) error { return nil }
