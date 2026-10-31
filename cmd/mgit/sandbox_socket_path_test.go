package main

import (
	"context"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// A unix socket's path must fit the platform's sun_path (104 bytes on macOS,
// 108 on Linux). The daemon's socket is <runtime base>/mgit-<uid>/<key>/d.sock
// and the sandbox's own sockets sit further below it. A long TMPDIR used to be
// refused here (MGIT-240) so the daemon was not spawned onto a path it could not
// bind; MGIT-279 supersedes the refusal: a base too long for the longest
// per-sandbox socket is replaced by a short fixed per-user dir, so the sandbox
// starts whatever TMPDIR's length. The refusal remains only as the backstop,
// when even the fallback cannot hold the paths. Refs: MGIT-240, MGIT-279
func TestResolveSandboxPaths_ALongBaseFallsBackAndTheBackstopStillRefuses(t *testing.T) {
	repo := newRepo(t)
	short := shortBaseFor(t)
	long := filepath.Join(t.TempDir(), strings.Repeat("x", 120))
	require.NoError(t, os.MkdirAll(long, 0o700))

	for _, tt := range []struct{ name, env string }{
		{"TMPDIR", "TMPDIR"},
		{"XDG_RUNTIME_DIR", "XDG_RUNTIME_DIR"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			t.Setenv("XDG_RUNTIME_DIR", "")
			t.Setenv(tt.env, long)
			p, err := resolveSandboxPaths(repo)
			require.NoError(t, err, "a long base no longer stops the sandbox")
			assert.True(t, strings.HasPrefix(p.socket, short), "the daemon socket moved to the short fixed dir: %s", p.socket)
			assert.LessOrEqual(t, len(p.socket), maxSocketPathBytes)
			_, statErr := os.Stat(filepath.Join(long, "mgit-"+strconv.Itoa(os.Getuid())))
			assert.True(t, os.IsNotExist(statErr), "nothing is made under the long base")
		})
	}

	// The backstop: if even the fallback is too long, the refusal of MGIT-240
	// still names the length, the limit and the variable that chose the directory.
	shortRuntimeBase = long
	t.Setenv("XDG_RUNTIME_DIR", "")
	t.Setenv("TMPDIR", long)
	_, err := resolveSandboxPaths(repo)
	require.Error(t, err, "a socket path over the limit cannot be bound")
	msg := err.Error()
	assert.Contains(t, msg, "bytes")
	assert.Contains(t, msg, strconv.Itoa(maxSocketPathBytes), "the platform's limit is named")
	assert.Contains(t, msg, "TMPDIR", "and the variable that chose the directory")
	_, err = sandboxConnectFor(context.Background(), repo)
	require.Error(t, err)
	assert.NotContains(t, err.Error(), "not dialable after spawn", "no daemon is spawned to find out")

	// Positive control: a short base is accepted and left where it is.
	shortRuntimeBase = short
	t.Setenv("XDG_RUNTIME_DIR", "")
	t.Setenv("TMPDIR", "/tmp")
	p, err := resolveSandboxPaths(repo)
	require.NoError(t, err)
	assert.LessOrEqual(t, len(p.socket), maxSocketPathBytes)
}
