//go:build linux

package firecracker

import (
	"context"
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/hyper-swe/mgit/internal/model"
	"github.com/hyper-swe/mgit/internal/reviewguest"
	"github.com/hyper-swe/mgit/internal/reviewprep"
	"github.com/hyper-swe/mgit/internal/sandboxd"
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

	lease := filepath.Join(t.TempDir(), "lease")
	require.NoError(t, os.Mkdir(lease, 0o700))
	require.NoError(t, os.WriteFile(filepath.Join(lease, "evidence.json"), []byte(`{"k":"v"}`), 0o400))

	uid, gid := os.Getuid(), os.Getgid()
	ctx := context.Background()
	g, err := reviewguest.Provision(ctx, mgr, reviewguest.Spec{
		Backend: model.BackendKVM, LeaseDir: lease, ImageRef: ref, TaskID: "REV-LIVE", UID: uid, GID: gid,
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

	sum := exec("/bin/busybox", "sha256sum", "/bin/busybox")
	require.Zero(t, sum.ExitCode, string(sum.Stderr))
	digest := strings.Fields(string(sum.Stdout))[0]

	// Control: the same program, same user, no launcher.
	ctl := exec("/bin/busybox", "touch", filepath.Join(lease, "control-marker"))
	require.Zero(t, ctl.ExitCode, "the control must be able to write, or the denial below proves nothing: %s", ctl.Stderr)

	// Through the launcher.
	res, err := g.Run(ctx, reviewguest.Entry{SHA256: digest, Path: "/bin/busybox",
		Argv: []string{"touch", filepath.Join(lease, "review-marker")}})
	require.NoError(t, err)
	assert.NotZero(t, res.ExitCode, "the reviewer's write must fail")

	ls := exec("/bin/busybox", "ls", lease)
	require.Zero(t, ls.ExitCode, string(ls.Stderr))
	assert.Contains(t, string(ls.Stdout), "control-marker")
	assert.NotContains(t, string(ls.Stdout), "review-marker")
	assert.Contains(t, string(ls.Stdout), "evidence.json")

	// The host's directory is untouched either way: the image was built at launch.
	for _, n := range []string{"control-marker", "review-marker"} {
		_, statErr := os.Stat(filepath.Join(lease, n))
		assert.True(t, errors.Is(statErr, os.ErrNotExist), "%s reached the host", n)
	}

	// A wrong digest is a launcher refusal that started nothing.
	_, err = g.Run(ctx, reviewguest.Entry{SHA256: strings.Repeat("0", 64), Path: "/bin/busybox", Argv: []string{"true"}})
	var re *reviewprep.Error
	require.ErrorAs(t, err, &re)
	assert.Equal(t, "HASH_MISMATCH", re.Code)
	assert.False(t, re.ReviewerStarted)
}
