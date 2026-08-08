package worktreesync

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// m builds a manifest from path->hash pairs; the mode is irrelevant to the
// classification and is held constant.
func m(pairs ...string) Manifest {
	out := Manifest{}
	for i := 0; i+1 < len(pairs); i += 2 {
		out[pairs[i]] = Entry{Hash: pairs[i+1], Mode: 0o644}
	}
	return out
}

// TestCompute_HostChanged_GuestUntouched_IsUpdated is the capability being
// added: a host edit between two execs reaches the guest. Refs: MGIT-71
func TestCompute_HostChanged_GuestUntouched_IsUpdated(t *testing.T) {
	plan := Compute(m("a.go", "v1"), m("a.go", "v2"), m("a.go", "v1"))

	assert.Equal(t, []string{"a.go"}, plan.Update)
	assert.Empty(t, plan.Delete)
	assert.Empty(t, plan.Conflicts)
	assert.False(t, plan.Empty())
}

// TestCompute_HostChanged_GuestModified_IsAConflict is the collision policy:
// un-landed guest work is never silently clobbered. Refs: MGIT-71
func TestCompute_HostChanged_GuestModified_IsAConflict(t *testing.T) {
	plan := Compute(m("a.go", "v1"), m("a.go", "v2"), m("a.go", "guest-edit"))

	require.Len(t, plan.Conflicts, 1)
	assert.Equal(t, "a.go", plan.Conflicts[0].Path)
	assert.Equal(t, ReasonModifiedInGuest, plan.Conflicts[0].Reason)
	assert.Empty(t, plan.Update, "nothing is applied when a conflict exists")
	assert.True(t, plan.Blocked())
}

// TestCompute_GuestCreatedPaths_AreNeverTouched is the class that keeps the
// agent loop viable: node_modules and build caches are guest-created and must
// survive every sync. A naive "make the guest match the host" would delete
// them on every round — destroying exactly what MGIT-73 exists to preserve.
// Refs: MGIT-71, MGIT-73
func TestCompute_GuestCreatedPaths_AreNeverTouched(t *testing.T) {
	plan := Compute(
		m("a.go", "v1"),
		m("a.go", "v2"),
		m("a.go", "v1", "node_modules/left-pad/index.js", "whatever", "build/out.o", "x"),
	)

	assert.Equal(t, []string{"a.go"}, plan.Update)
	assert.Empty(t, plan.Delete, "a guest-created path is never deleted")
	assert.Empty(t, plan.Conflicts, "a guest-created path is not a conflict either")
}

// TestCompute_HostDeleted_GuestUntouched_IsDeleted verifies a host deletion
// propagates when the guest never touched the file.
func TestCompute_HostDeleted_GuestUntouched_IsDeleted(t *testing.T) {
	plan := Compute(m("gone.go", "v1"), m(), m("gone.go", "v1"))

	assert.Equal(t, []string{"gone.go"}, plan.Delete)
	assert.Empty(t, plan.Conflicts)
}

// TestCompute_HostDeleted_GuestModified_IsAConflict verifies a host deletion
// does not destroy guest work either — deletion is as destructive as
// overwriting.
func TestCompute_HostDeleted_GuestModified_IsAConflict(t *testing.T) {
	plan := Compute(m("gone.go", "v1"), m(), m("gone.go", "guest-edit"))

	require.Len(t, plan.Conflicts, 1)
	assert.Equal(t, ReasonModifiedInGuest, plan.Conflicts[0].Reason)
	assert.Empty(t, plan.Delete)
}

// TestCompute_HostAdded_CollidingWithAGuestCreatedPath_IsAConflict verifies a
// new host file does not silently overwrite a guest file of the same name.
func TestCompute_HostAdded_CollidingWithAGuestCreatedPath_IsAConflict(t *testing.T) {
	plan := Compute(m(), m("new.go", "v1"), m("new.go", "guest-made-this"))

	require.Len(t, plan.Conflicts, 1)
	assert.Equal(t, ReasonCreatedInGuest, plan.Conflicts[0].Reason)
	assert.Empty(t, plan.Update)
}

// TestCompute_HostAdded_NotPresentInGuest_IsUpdated verifies a genuinely new
// host file is delivered.
func TestCompute_HostAdded_NotPresentInGuest_IsUpdated(t *testing.T) {
	plan := Compute(m(), m("new.go", "v1"), m())

	assert.Equal(t, []string{"new.go"}, plan.Update)
	assert.Empty(t, plan.Conflicts)
}

// TestCompute_HostUnchanged_IsANoOp verifies an unchanged host worktree costs
// nothing, whatever the guest has done to its own copy. This is what makes an
// automatic pre-exec sync affordable.
func TestCompute_HostUnchanged_IsANoOp(t *testing.T) {
	plan := Compute(m("a.go", "v1"), m("a.go", "v1"), m("a.go", "guest-edit"))

	assert.True(t, plan.Empty())
	assert.Empty(t, plan.Conflicts, "the guest may edit freely; sync only carries HOST changes")
}

// TestCompute_ModeChangeIsAChange verifies a permission change counts (an
// executable bit is a real edit).
func TestCompute_ModeChangeIsAChange(t *testing.T) {
	delivered := Manifest{"run.sh": {Hash: "v1", Mode: 0o644}}
	host := Manifest{"run.sh": {Hash: "v1", Mode: 0o755}}
	guest := Manifest{"run.sh": {Hash: "v1", Mode: 0o644}}

	plan := Compute(delivered, host, guest)

	assert.Equal(t, []string{"run.sh"}, plan.Update)
}

// TestCompute_ConflictsAreOrdered verifies conflict output is deterministic,
// so the refusal message a caller sees is stable and diffable.
func TestCompute_ConflictsAreOrdered(t *testing.T) {
	plan := Compute(
		m("b.go", "v1", "a.go", "v1", "c.go", "v1"),
		m("b.go", "v2", "a.go", "v2", "c.go", "v2"),
		m("b.go", "g", "a.go", "g", "c.go", "g"),
	)

	require.Len(t, plan.Conflicts, 3)
	assert.Equal(t, "a.go", plan.Conflicts[0].Path)
	assert.Equal(t, "b.go", plan.Conflicts[1].Path)
	assert.Equal(t, "c.go", plan.Conflicts[2].Path)
}

// TestCompute_UpdatesAndDeletesAreOrdered verifies the applied work is
// deterministic too.
func TestCompute_UpdatesAndDeletesAreOrdered(t *testing.T) {
	plan := Compute(
		m("b.go", "v1", "a.go", "v1", "z.go", "v1", "y.go", "v1"),
		m("b.go", "v2", "a.go", "v2"),
		m("b.go", "v1", "a.go", "v1", "z.go", "v1", "y.go", "v1"),
	)

	assert.Equal(t, []string{"a.go", "b.go"}, plan.Update)
	assert.Equal(t, []string{"y.go", "z.go"}, plan.Delete)
}

// TestCompute_Force_OverridesConflicts verifies --force converts conflicts
// into updates, and still REPORTS them so every overwritten path can be
// audited rather than vanishing silently. Refs: MGIT-71
func TestCompute_Force_OverridesConflicts(t *testing.T) {
	plan := Compute(m("a.go", "v1"), m("a.go", "v2"), m("a.go", "guest-edit"))
	require.True(t, plan.Blocked())

	forced := plan.Forced()

	assert.False(t, forced.Blocked())
	assert.Equal(t, []string{"a.go"}, forced.Update)
	assert.Len(t, forced.Conflicts, 1, "the overwritten paths stay reported for the audit record")
	assert.Equal(t, []string{"a.go"}, forced.Overridden())
}

// TestCompute_PrivateStoreIsNeverSynced verifies the guest's own .mgit store
// is out of scope entirely: the guest commits into it, and a sync that
// overwrote it would destroy exactly the work land exists to carry out.
// Refs: MGIT-71, SEC-03
func TestCompute_PrivateStoreIsNeverSynced(t *testing.T) {
	plan := Compute(
		m(".mgit/HEAD", "v1", "a.go", "v1"),
		m(".mgit/HEAD", "v2", "a.go", "v2"),
		m(".mgit/HEAD", "guest-commit", "a.go", "v1"),
	)

	assert.Equal(t, []string{"a.go"}, plan.Update, "only the worktree file syncs")
	assert.Empty(t, plan.Conflicts, "the private store is not a conflict; it is out of scope")
}
