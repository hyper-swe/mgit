//go:build unix

package worktreesync

import (
	"errors"
	"os"
	"path/filepath"
	"syscall"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestONoFollow_RefusesALinkAsTheFinalComponent pins the platform primitive
// emptyRegularFile relies on. Its guarantee against a concurrent swap is only
// as good as O_NOFOLLOW's behavior on the host it runs on: a platform that
// silently followed the link would leave the guard in place and inert, which
// is the kind of silence nothing else here would notice. So the primitive is
// asserted directly — the link is refused with ELOOP and its target is not
// opened, let alone emptied. Refs: MGIT-168
func TestONoFollow_RefusesALinkAsTheFinalComponent(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "real.txt")
	require.NoError(t, os.WriteFile(target, []byte("KEEP-THIS"), 0o600))
	link := filepath.Join(dir, "link.txt")
	require.NoError(t, os.Symlink("real.txt", link))

	f, err := os.OpenFile(link, os.O_WRONLY|oNoFollow, 0) //nolint:gosec // a t.TempDir path this test wrote
	if err == nil {
		_ = f.Close()
	}
	require.Error(t, err, "O_NOFOLLOW must refuse to open through a link")
	assert.True(t, errors.Is(err, syscall.ELOOP), "the refusal is ELOOP, the code emptyRegularFile treats as 'not a regular file': %v", err)
	assert.Equal(t, "KEEP-THIS", readFile(t, target))
}
