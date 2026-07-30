package images

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"
)

// TestEnsureSigningKey_CreatesOnceThenReuses covers the property that makes
// first-run work without making later runs dangerous.
//
// Every command that signs into images.lock needs a trust root, and a user
// who has never run `image init` has none. Generating one on demand is what
// `image install` already did; doing it in only some of the sibling commands
// is how a user gets told to run a command that then tells them to run
// another one.
//
// The second half is the part that must never regress: an EXISTING key is
// reused, never rotated. Rotating would invalidate every image already
// registered under it, turning a convenience into data loss.
// Refs: MGIT-65, FR-17.38
func TestEnsureSigningKey_CreatesOnceThenReuses(t *testing.T) {
	hostRoot := t.TempDir()
	rec := &recordingAudit{}

	first, err := EnsureSigningKey(context.Background(), hostRoot, rec)
	require.NoError(t, err, "a repo with no trust root must get one")
	require.NotEmpty(t, first)
	require.Len(t, rec.details, 1, "creating a trust root is an audited event")

	second, err := EnsureSigningKey(context.Background(), hostRoot, rec)

	require.NoError(t, err)
	require.Equal(t, first, second,
		"an existing key must be reused; rotating it would invalidate every registered image")
	require.Len(t, rec.details, 1, "reuse is not a trust-root change and must not be audited as one")
}
