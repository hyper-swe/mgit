// Package container e2e: the exec identity must be usable, not just recorded.
// Applying the requested identity is only correct if that identity can still
// write the worktree; the launch maps the daemon's host identity to the same
// identity inside so that it can. This is env-gated exactly like the
// hostile-guest e2e (podman on PATH + MGIT_E2E_CONTAINER_IMAGE). Refs: MGIT-273
package container

import (
	"context"
	"log/slog"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/hyper-swe/mgit/internal/model"
)

// TestE2E_Container_ExecIdentity_WritesWorktree launches a real container and
// checks, live, that a command run as the daemon's identity can write the
// mounted worktree (the property the launch mapping exists for), and records
// what an audited request for a different identity can do. Refs: MGIT-273
func TestE2E_Container_ExecIdentity_WritesWorktree(t *testing.T) {
	image := requirePodman(t)
	_, wtPath, prov := setupContainerHostileFixture(t)

	mgr, err := NewManager(Config{
		Runner:           PodmanRunner{},
		WorkDir:          t.TempDir(),
		StoreProvisioner: prov,
		SensitivePaths:   model.DefaultSandboxPolicy().SensitivePaths,
		Logger:           slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError})),
		Clock:            func() time.Time { return time.Now().UTC() },
	})
	require.NoError(t, err)

	info, err := mgr.Launch(context.Background(), model.SandboxLaunchOptions{
		TaskID: hostileTask, WorktreePath: wtPath, ImageRef: image,
		Network: model.NetworkPolicy{Mode: model.NetworkModeNone}, CPUs: 1, MemoryMB: 256,
	})
	require.NoError(t, err)
	t.Cleanup(func() { _ = mgr.Remove(context.Background(), info.ID, true) })

	// The daemon runs execs as its own host identity; the service fills RunAs
	// with it. The launch mapped it to the same identity inside, so it must be
	// able to write the worktree.
	daemon := model.IdentityForProcess(os.Getuid(), os.Getgid())
	res, err := mgr.Exec(context.Background(), info.ID, model.ExecRequest{
		Command: []string{"/bin/sh", "-c",
			"id -u; touch " + filepath.Join(wtPath, "probe") + " && echo wrote || echo denied"},
		RunAs: &daemon,
	})
	require.NoError(t, err)
	out := string(res.Stdout)
	t.Logf("as the daemon identity: %q", strings.TrimSpace(out))
	// id -u's first line is the identity the command actually ran as, IN the
	// container — the teeth: under the launch mapping a missing --user would
	// still run as the default and write, so asserting the write alone would
	// not catch it; asserting the effective id does.
	assert.Equal(t, strconv.Itoa(os.Getuid()), firstLine(out),
		"the command ran, in the container, as the requested identity")
	assert.Contains(t, out, "wrote",
		"a command run as the daemon identity can write the mounted worktree")
	require.NotNil(t, res.RanAs)
	assert.Equal(t, os.Getuid(), res.RanAs.UID, "the result reports the identity it ran as")

	// An audited request for a different (elevated) identity also runs as it
	// and writes the worktree (measured; the launch mapping keeps the elevated
	// identity able to write). Refs: MGIT-273
	elevated := model.RootIdentity()
	rootRes, err := mgr.Exec(context.Background(), info.ID, model.ExecRequest{
		Command: []string{"/bin/sh", "-c",
			"id -u; touch " + filepath.Join(wtPath, "probe-elevated") + " && echo wrote || echo denied"},
		RunAs: &elevated,
	})
	require.NoError(t, err)
	rootOut := string(rootRes.Stdout)
	t.Logf("as an audited elevated identity: %q", strings.TrimSpace(rootOut))
	assert.Equal(t, strconv.Itoa(elevated.UID), firstLine(rootOut),
		"the command ran, in the container, as the requested elevated identity")
	assert.Contains(t, rootOut, "wrote",
		"an audited elevated identity can also write the mounted worktree")
}

// firstLine returns the first line of s, trimmed.
func firstLine(s string) string {
	return strings.TrimSpace(strings.SplitN(strings.TrimSpace(s), "\n", 2)[0])
}
