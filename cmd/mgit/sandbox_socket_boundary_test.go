package main

import (
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// The limit is sun_path less its NUL, so a socket path of exactly
// maxSocketPathBytes binds and one byte more cannot. The fallback decision is
// made on the longest PER-SANDBOX socket below the base, so the base is padded
// until that path lands on the limit exactly: at the limit the base is kept
// and the path is really bound; one byte over, the short fixed base is used.
// Bases far over and far under the limit cannot tell `<` from `<=`, or a limit
// that counts the NUL; these two lengths can. Refs: MGIT-240.1, MGIT-279
func TestResolveSandboxPaths_TheFallbackBoundaryIsInclusive(t *testing.T) {
	repo := newRepo(t)
	short := shortBaseFor(t)
	root, err := os.MkdirTemp("/tmp", "sl")
	require.NoError(t, err)
	t.Cleanup(func() { _ = os.RemoveAll(root) }) // a scratch dir this test made

	base := func(total int) string {
		pad := total - sandboxSocketTailLen() - len(root) - 1
		require.Positive(t, pad, "the scratch root leaves room to pad")
		b := filepath.Join(root, strings.Repeat("p", pad))
		require.NoError(t, os.MkdirAll(b, 0o700))
		return b
	}
	t.Setenv("XDG_RUNTIME_DIR", "")

	t.Setenv("TMPDIR", base(maxSocketPathBytes))
	p, err := resolveSandboxPaths(repo)
	require.NoError(t, err)
	assert.False(t, strings.HasPrefix(p.socket, short), "a base whose longest sandbox socket fits exactly is kept: %s", p.socket)
	worst := filepath.Join(p.workDir, "abcdefgh", "v65535.sock")
	assert.Len(t, worst, maxSocketPathBytes, "the fixture lands on the limit exactly")
	dir := filepath.Dir(worst)
	require.NoError(t, os.MkdirAll(dir, 0o700))
	// The platform's own answer, which this code does not control: a path of
	// exactly the limit really binds. A limit that counted the NUL would put
	// this path one byte past what macOS can bind, and this fails there.
	ln, err := net.Listen("unix", worst)
	require.NoError(t, err, "the kernel binds a socket path of exactly the limit")
	_ = ln.Close() // a listener this test opened only to prove the bind

	t.Setenv("TMPDIR", base(maxSocketPathBytes+1))
	p, err = resolveSandboxPaths(repo)
	require.NoError(t, err)
	assert.True(t, strings.HasPrefix(p.socket, short), "one byte over, the short fixed base is used: %s", p.socket)
}
