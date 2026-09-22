package worktreesync

import (
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestSync_GuestOnlyEdit_IsKeptEvenUnderForce pins ADR-011's last rule at the
// syncer: a path only the guest changed is kept by a sync from an unchanged
// host — under --force too, because --force overrides CONFLICTS and a conflict
// needs a host change. The help text once let a reader expect a restore
// (FEAT-6.37); this is the behavior the text now describes, and the second
// half shows the documented discard path: change the same path on the host,
// and --force wins. Refs: MGIT-193, MGIT-71, ADR-011
func TestSync_GuestOnlyEdit_IsKeptEvenUnderForce(t *testing.T) {
	f := newFixture(t, map[string]string{"app.go": "V1", "doc.md": "D1"})
	writeTree(t, f.guestTree, map[string]string{"app.go": "GUEST-EDIT"})

	for _, force := range []bool{false, true} {
		res, err := f.sync(force)
		require.NoError(t, err)
		assert.True(t, res.Skipped, "force=%v: an unchanged host is a no-op, whatever the guest did", force)
		assert.False(t, res.Changed())
		assert.Empty(t, res.Conflicts)
		assert.Empty(t, res.Overridden)
		assert.Equal(t, "GUEST-EDIT", readFile(t, filepath.Join(f.guestTree, "app.go")),
			"force=%v: the guest's edit is kept", force)
	}

	// The discard path the help names: a host change to the SAME path makes
	// it a conflict, refused unforced and overwritten under --force.
	writeTree(t, f.worktree, map[string]string{"app.go": "V1-resaved"})
	_, err := f.sync(false)
	var ce *ConflictError
	require.ErrorAs(t, err, &ce, "both sides changed: refused without --force")
	res, err := f.sync(true)
	require.NoError(t, err)
	assert.Equal(t, []string{"app.go"}, res.Overridden)
	assert.Equal(t, "V1-resaved", readFile(t, filepath.Join(f.guestTree, "app.go")))
}
