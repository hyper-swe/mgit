//go:build linux

package firecracker

import (
	"context"
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
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
	"github.com/hyper-swe/mgit/internal/sandboxd/reviewsvc"
)

// TestE2E_ReviewGuest_LaunchTimeImage provisions the review guest on a real
// firecracker VM (the launch-time-image backend) and runs one pinned program
// through the launcher. The same program, run directly as the same user in the
// same guest, creates a file; through the launcher it cannot. That pair is what
// makes the denial a property of the launcher rather than of the image.
// Refs: FEAT-3.347.1, MGIT-333
func TestE2E_ReviewGuest_LaunchTimeImage(t *testing.T) {
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

	// The reviewer is never root. Unprivileged, it is the caller's own identity;
	// run as root (the gate's second half) it is a fixed unprivileged one, and
	// the lease is handed to it so it can read what it was given.
	uid, gid := os.Getuid(), os.Getgid()
	if uid == 0 {
		uid, gid = 1001, 1001
	}
	ctx := context.Background()
	g, err := reviewguest.Provision(ctx, mgr, reviewguest.Spec{
		Backend: model.BackendKVM, Lease: lease, TaskID: "REV-LIVE", UID: uid, GID: gid,
	})
	require.NoError(t, err)
	t.Cleanup(func() { _ = g.Close(context.Background()) })

	if os.Getuid() == 0 {
		// Delivered as the daemon's own identity (root here); give the
		// guest's copy to the unprivileged reviewer so it can read what it
		// was given. This changes the guest's image only, never the host.
		own, oerr := mgr.Exec(ctx, g.ID, model.ExecRequest{
			Command: []string{"/bin/busybox", "chown", "-R", fmt.Sprintf("%d:%d", uid, gid), leaseDir}, Timeout: 30 * time.Second,
		})
		require.NoError(t, oerr)
		require.Zero(t, own.ExitCode, string(own.Stderr))
	}
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
		// Say what the guest was doing, so a hang is a diagnosis and not a
		// timeout: run the same launch again from a script that, if it is still
		// running after 20s, prints every process's state, wait channel,
		// syscall and kernel stack, and asks the launcher processes for a Go
		// goroutine dump (SIGQUIT) before reading what they wrote.
		script := fmt.Sprintf(hangProbe, leaseDir, digest, uid, gid)
		d, derr := mgr.Exec(ctx, g.ID, model.ExecRequest{Command: []string{"/bin/sh", "-c", script}, RunAs: nil, Timeout: 90 * time.Second})
		if derr != nil {
			t.Logf("diagnostic exec failed: %v", derr)
		} else {
			t.Logf("diagnostic exit %d\n%s\n%s", d.ExitCode, d.Stdout, d.Stderr)
		}
	}
	require.NoError(t, err)
	assert.NotZero(t, res.ExitCode, "the reviewer's write must fail")
	// The failure is the denial itself (EPERM from the filter), not some other
	// reason the program could not write.
	assert.Contains(t, string(res.Stderr), "Operation not permitted", "the write must be refused with EPERM")
	assert.NotContains(t, string(res.Stderr), "No such file", "the write must not fail for another reason")

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

// hangProbe is run in the guest by busybox sh with: lease dir, entrypoint
// digest, uid, gid.
const hangProbe = `B=/bin/busybox; L=%s; D=%s; U=%d; G=%d
/sbin/mgit-guest review-exec --sha256 $D --uid $U --gid $G --dir $L -- /bin/busybox touch $L/diag-marker >/tmp/o 2>/tmp/e &
P=$!
i=0
while kill -0 $P 2>/dev/null && [ $i -lt 20 ]; do sleep 1; i=$((i+1)); done
if kill -0 $P 2>/dev/null; then
  echo "STILL RUNNING after ${i}s; launcher pid $P"
  $B ps -o pid,ppid,user,stat,args 2>&1 | $B grep -v '\[' 
  for d in /proc/[0-9]*; do
    p=${d#/proc/}
    [ "$p" = 1 ] && continue
    [ -e $d/exe ] || continue
    echo "== pid $p exe=$($B readlink $d/exe) comm=$(cat $d/comm)"
    $B grep -E '^(State|PPid|Uid|NoNewPrivs|Seccomp)' $d/status 2>&1 | $B tr '\n' ' '; echo
    echo "wchan=$(cat $d/wchan 2>&1) syscall=$(cat $d/syscall 2>&1)"
    head -8 $d/stack 2>&1
    echo "fds: $(ls -l $d/fd 2>&1 | $B sed 's/.* [0-9:]* //' | $B tr '\n' ' ')"
  done
  for d in /proc/[0-9]*; do
    p=${d#/proc/}
    [ "$p" = 1 ] && continue
    case "$($B readlink $d/exe 2>/dev/null)" in */mgit-guest) $B kill -QUIT $p 2>/dev/null;; esac
  done
  sleep 3
  echo "== launcher stderr"; head -c 8000 /tmp/e
else
  wait $P; echo "RETURNED rc=$? within ${i}s"; cat /tmp/o; head -c 2000 /tmp/e
fi`
