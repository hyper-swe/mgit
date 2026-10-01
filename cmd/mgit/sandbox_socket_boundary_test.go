package main

import (
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// The limit is sun_path less its NUL, so a socket path of exactly
// maxSocketPathBytes binds and one byte more cannot. Bases far over and far
// under the limit cannot tell `<` from `<=`, or a limit that counts the NUL;
// these two lengths can. The base is padded so the whole socket path lands on
// each length exactly, and the path at the limit is really bound.
// Refs: MGIT-240.1, MGIT-240
func TestResolveSandboxPaths_TheLimitIsInclusiveAndOneMoreIsRefused(t *testing.T) {
	repo := newRepo(t)
	// <base>/mgit-<uid>/<12 hex>/d.sock
	suffix := len(fmt.Sprintf("/mgit-%d/", os.Getuid())) + 12 + len("/d.sock")
	root, err := os.MkdirTemp("/tmp", "sl")
	require.NoError(t, err)
	t.Cleanup(func() { _ = os.RemoveAll(root) }) // a scratch dir this test made

	base := func(total int) string {
		pad := total - suffix - len(root) - 1
		require.Positive(t, pad, "the scratch root leaves room to pad")
		b := filepath.Join(root, strings.Repeat("p", pad))
		require.NoError(t, os.MkdirAll(b, 0o700))
		return b
	}
	t.Setenv("XDG_RUNTIME_DIR", "")

	t.Setenv("TMPDIR", base(maxSocketPathBytes))
	p, err := resolveSandboxPaths(repo)
	require.NoError(t, err, "a socket path of exactly the limit binds")
	assert.Len(t, p.socket, maxSocketPathBytes, "the fixture lands on the limit exactly")
	// The platform's own answer, which this code does not control: a path of
	// exactly the limit really binds. A limit that counted the NUL would put
	// this path one byte past what macOS can bind, and this fails there.
	ln, err := net.Listen("unix", p.socket)
	require.NoError(t, err, "the kernel binds a socket path of exactly the limit")
	_ = ln.Close() // a listener this test opened only to prove the bind

	t.Setenv("TMPDIR", base(maxSocketPathBytes+1))
	_, err = resolveSandboxPaths(repo)
	require.Error(t, err, "one byte over the limit cannot bind")
	assert.Contains(t, err.Error(), fmt.Sprintf("is %d bytes", maxSocketPathBytes+1))
}
