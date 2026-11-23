//go:build linux

package firecracker

import (
	"context"
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
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
	"github.com/hyper-swe/mgit/internal/sandboxd/backend/microvm"
	"github.com/hyper-swe/mgit/internal/sandboxd/reviewsvc"
)

// TestE2E_CredentialSlot_IsMemoryBackedAndGroupReadable installs a made-up
// credential in a real guest through the daemon-internal stdin channel and
// checks what the guest actually did with it: the slot lives on a tmpfs (never
// on the root image), the reviewer's group reads it, another user does not, it
// is not in the install's own argv or output, removal leaves nothing, and an
// install whose stdin is empty fails closed. No real key is used.
// Refs: FEAT-3.347.1.5
func TestE2E_CredentialSlot_IsMemoryBackedAndGroupReadable(t *testing.T) {
	kernel, rootfs := requireE2EGuest(t)
	clock := func() time.Time { return time.Now().UTC() }
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
	mgr, ref, _ := e2eBootGuest(t, kernel, rootfs, sandboxd.NewPeerBinder(logger), clock, logger)
	ctx := context.Background()
	id := e2eReviewGuest(t, mgr, ref, clock)
	const secret = "FAKE-KEY-0123456789-not-real"
	const dir, slot = "/run/review-cred", "/run/review-cred/slot1"
	const guestBin = "/sbin/mgit-guest"
	const gid, other = 4242, 4343

	run := func(stdin string, cmd ...string) *model.ExecResult {
		t.Helper()
		res, err := mgr.Exec(ctx, id, model.ExecRequest{Command: cmd, Stdin: model.Secret(stdin), Timeout: 60 * time.Second})
		require.NoError(t, err)
		return res
	}
	as := func(uid, g int, cmd ...string) *model.ExecResult {
		t.Helper()
		res, err := mgr.Exec(ctx, id, model.ExecRequest{Command: cmd, RunAs: &model.GuestIdentity{UID: uid, GID: g}, Timeout: 60 * time.Second})
		require.NoError(t, err)
		return res
	}

	// An install with nothing on stdin installs nothing (a guest that dropped stdin looks like this).
	empty := run("", guestBin, "cred-install", "--dest", slot, "--gid", fmt.Sprint(gid))
	assert.NotZero(t, empty.ExitCode, "an empty stdin must be refused")
	gone := run("", "/bin/busybox", "ls", slot)
	assert.NotZero(t, gone.ExitCode, "no slot may exist after the refusal")

	in := run(secret, guestBin, "cred-install", "--dest", slot, "--gid", fmt.Sprint(gid))
	require.Zero(t, in.ExitCode, "install: %s", in.Stderr)
	assert.NotContains(t, string(in.Stdout)+string(in.Stderr), secret, "the install must not echo the secret")

	// The slot is on a tmpfs, not on the root image.
	mnt := run("", "/bin/busybox", "mount")
	require.Zero(t, mnt.ExitCode, string(mnt.Stderr))
	var onTmpfs bool
	for _, l := range strings.Split(string(mnt.Stdout), "\n") {
		f := strings.Fields(l)
		if len(f) >= 5 && f[2] == dir && f[4] == "tmpfs" {
			onTmpfs = true
		}
	}
	assert.True(t, onTmpfs, "%s must be a tmpfs mount; mount says:\n%s", dir, mnt.Stdout)

	// Root-owned, group-readable, nobody else.
	ls := run("", "/bin/busybox", "ls", "-ln", slot)
	require.Zero(t, ls.ExitCode, string(ls.Stderr))
	f := strings.Fields(string(ls.Stdout))
	require.GreaterOrEqual(t, len(f), 4, string(ls.Stdout))
	assert.Equal(t, "-r--r-----", f[0])
	assert.Equal(t, "0", f[2])
	assert.Equal(t, fmt.Sprint(gid), f[3])

	// Say what the path looks like to the reader, so a refusal is a diagnosis.
	t.Logf("path modes:\n%s", run("", "/bin/busybox", "ls", "-ldn", "/", "/run", dir, slot).Stdout)

	// The reviewer's group reads it; another user cannot.
	rd := as(1001, gid, "/bin/busybox", "cat", slot)
	require.Zero(t, rd.ExitCode, "the reviewer's group must read the slot: %s", rd.Stderr)
	assert.Equal(t, secret, string(rd.Stdout))
	no := as(1002, other, "/bin/busybox", "cat", slot)
	assert.NotZero(t, no.ExitCode, "another user must not read the slot")
	assert.NotContains(t, string(no.Stdout), secret)

	// A second install does not replace it.
	again := run("another", guestBin, "cred-install", "--dest", slot, "--gid", fmt.Sprint(gid))
	assert.NotZero(t, again.ExitCode)

	// Removal leaves nothing, and may run twice.
	rm := run("", guestBin, "cred-remove", "--dest", slot)
	require.Zero(t, rm.ExitCode, string(rm.Stderr))
	assert.NotZero(t, run("", "/bin/busybox", "ls", slot).ExitCode, "the slot must be gone")
	assert.Zero(t, run("", guestBin, "cred-remove", "--dest", slot).ExitCode, "a second removal is a no-op")
}

// e2eReviewGuest provisions a review guest the way the review service does (a
// sealed lease the daemon issued, a launch-time image) and returns its id. The
// guest is only a booted VM with the mgit-guest agent; this test then talks to
// it through the manager's Exec.
func e2eReviewGuest(t *testing.T, mgr *microvm.Manager, ref string, clock func() time.Time) string {
	t.Helper()
	old := reviewprep.WriteDenialEnforced
	reviewprep.WriteDenialEnforced = true
	t.Cleanup(func() { reviewprep.WriteDenialEnforced = old })
	hostRoot := t.TempDir()
	require.NoError(t, attest.GenerateKey(context.Background(), hostRoot, reviewKeyAudit{}))
	mapping := reviewprep.Mapping{
		Schema: reviewprep.MapSchema, ID: "live-cred", BindingSHA256: strings.Repeat("b", 64), ImageDigest: imageDigestOf(ref),
		GuestEntrypoint: "/bin/busybox", EntrypointSHA256: strings.Repeat("e", 64), Argv: []string{"true"}, EgressPolicyID: "none",
	}
	mraw, _ := json.Marshal(mapping)
	require.NoError(t, os.MkdirAll(filepath.Join(hostRoot, "review", "maps"), 0o700))
	require.NoError(t, os.WriteFile(filepath.Join(hostRoot, "review", "maps", "live-cred.json"), mraw, 0o600))
	bundle := t.TempDir()
	evidence := []byte(`{"k":"v"}`)
	require.NoError(t, os.WriteFile(filepath.Join(bundle, "evidence.json"), evidence, 0o600))
	sum := sha256.Sum256(evidence)
	args := controlproto.ReviewPrepareArgs{
		Profile: reviewprep.Profile, Image: ref, MappingID: "live-cred", BindingDigest: strings.Repeat("b", 64),
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
	g, err := reviewguest.Provision(context.Background(), mgr, reviewguest.Spec{
		Backend: model.BackendKVM, Lease: lease, TaskID: "REV-CRED", UID: 1001, GID: 1001,
	})
	require.NoError(t, err)
	t.Cleanup(func() { _ = g.Close(context.Background()) })
	return g.ID
}
