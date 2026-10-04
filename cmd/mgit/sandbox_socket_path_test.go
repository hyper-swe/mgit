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
// 108 on Linux, NUL included where the kernel wants one). The daemon's socket
// is <runtime base>/mgit-<uid>/<key>/d.sock, and the runtime base is
// XDG_RUNTIME_DIR or the temp dir, so a long TMPDIR made it 153 bytes. The
// daemon was spawned anyway, ran its start-up (including an attestation-key
// write), and died on `bind: invalid argument`; the CLI reported the socket
// "not dialable after spawn". The limit is known before the spawn, so the
// path is refused there, naming its length, the limit, and the variable
// that chose the directory. Refs: MGIT-240
func TestResolveSandboxPaths_ASocketPathOverThePlatformLimit_IsRefusedBeforeTheSpawn(t *testing.T) {
	repo := newRepo(t)
	long := filepath.Join(t.TempDir(), strings.Repeat("x", 120))
	require.NoError(t, os.MkdirAll(long, 0o700))

	for _, tt := range []struct{ name, env string }{
		{"TMPDIR", "TMPDIR"},
		{"XDG_RUNTIME_DIR", "XDG_RUNTIME_DIR"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			t.Setenv("XDG_RUNTIME_DIR", "")
			t.Setenv(tt.env, long)
			_, err := resolveSandboxPaths(repo)
			require.Error(t, err, "a socket path over the limit cannot be bound")
			msg := err.Error()
			assert.Contains(t, msg, "bytes")
			assert.Contains(t, msg, strconv.Itoa(maxSocketPathBytes), "the platform's limit is named")
			assert.Contains(t, msg, tt.env, "and the variable that chose the directory")
			_, statErr := os.Stat(filepath.Join(long, "mgit-"+strconv.Itoa(os.Getuid())))
			assert.True(t, os.IsNotExist(statErr), "refused before anything is made under it")

			_, err = sandboxConnectFor(context.Background(), repo)
			require.Error(t, err)
			assert.NotContains(t, err.Error(), "not dialable after spawn", "no daemon is spawned to find out")
		})
	}

	// Positive control: a short base is accepted, so the refusal is about
	// length and nothing else.
	t.Setenv("XDG_RUNTIME_DIR", "")
	t.Setenv("TMPDIR", "/tmp")
	p, err := resolveSandboxPaths(repo)
	require.NoError(t, err)
	assert.LessOrEqual(t, len(p.socket), maxSocketPathBytes)
}
