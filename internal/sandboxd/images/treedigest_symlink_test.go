package images

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// emptyInputDigest is the SHA-256 of nothing: a pin that covers no bytes.
const emptyInputDigest = "sha256:e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855"

// A PIN MUST COVER THE TREE IT NAMES (MGIT-227). TreeDigest stat'ed the root
// through a symlink (a directory, so accepted) but walked the link itself,
// which filepath.WalkDir does not follow: nothing was hashed, and the pin
// was the SHA-256 of empty input, which then verified whatever the link
// pointed at. A symlinked root now digests exactly as the tree behind it,
// and a symlink INSIDE that tree is still pinned by its target and checked
// against the real tree. Refs: MGIT-227, MGIT-61.15
func TestTreeDigest_ASymlinkedRootDigestsTheTreeBehindIt(t *testing.T) {
	tree := t.TempDir()
	require.NoError(t, os.MkdirAll(filepath.Join(tree, "bin"), 0o750))
	require.NoError(t, os.WriteFile(filepath.Join(tree, "bin", "busybox"), []byte("box"), 0o600))
	require.NoError(t, os.Symlink("busybox", filepath.Join(tree, "bin", "sh")))
	link := filepath.Join(t.TempDir(), "base-link")
	require.NoError(t, os.Symlink(tree, link))

	direct, err := TreeDigest(tree)
	require.NoError(t, err)
	viaLink, err := TreeDigest(link)
	require.NoError(t, err, "an inner symlink is judged against the real tree, not the link's path")

	assert.Equal(t, direct, viaLink, "a symlinked root digests the tree behind it")
	assert.NotEqual(t, emptyInputDigest, viaLink, "never the digest of nothing")
}

// A lock written before the fix may hold a vacuous pin. Now that the tree is
// really hashed, verification fails for it, which is right; the failure says
// why and what to run, instead of reading like a tampered base. Refs: MGIT-227
func TestVerifyContentDigest_AVacuousPinSaysItCoveredNothing(t *testing.T) {
	tree := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(tree, "f"), []byte("x"), 0o600))
	err := verifyContentDigest(tree, emptyInputDigest)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "covered no bytes")
	assert.Contains(t, err.Error(), "mgit sandbox base set")
}
