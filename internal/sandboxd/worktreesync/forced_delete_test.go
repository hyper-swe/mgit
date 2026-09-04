package worktreesync

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

// Under --force, a conflict is honored in the host's favor: an update when
// the host still has the path, a DELETE when the host no longer has it. The
// rows are the three conflict shapes ADR-011 names, not anything derived
// from Forced()'s own loop. Refs: MGIT-167, ADR-011
func TestForced_RoutesEachConflictShapeInTheHostsFavor(t *testing.T) {
	tests := []struct {
		name           string
		delivered      Manifest
		host           Manifest
		guest          Manifest
		wantUpdate     []string
		wantDelete     []string
		wantOverridden []string
	}{
		{
			name:           "host_deleted_guest_modified_is_a_forced_delete",
			delivered:      Manifest{"doomed.txt": {Hash: "v1"}},
			host:           Manifest{},
			guest:          Manifest{"doomed.txt": {Hash: "guest"}},
			wantUpdate:     []string{},
			wantDelete:     []string{"doomed.txt"},
			wantOverridden: []string{"doomed.txt"},
		},
		{
			name:           "host_changed_guest_modified_is_a_forced_update",
			delivered:      Manifest{"app.go": {Hash: "v1"}},
			host:           Manifest{"app.go": {Hash: "v2"}},
			guest:          Manifest{"app.go": {Hash: "guest"}},
			wantUpdate:     []string{"app.go"},
			wantDelete:     []string{},
			wantOverridden: []string{"app.go"},
		},
		{
			name:           "host_added_over_a_guest_created_name_is_a_forced_update",
			delivered:      Manifest{},
			host:           Manifest{"new.go": {Hash: "v1"}},
			guest:          Manifest{"new.go": {Hash: "guest"}},
			wantUpdate:     []string{"new.go"},
			wantDelete:     []string{},
			wantOverridden: []string{"new.go"},
		},
		{
			name:           "mixed_the_delete_and_the_update_land_in_their_own_lists",
			delivered:      Manifest{"doomed.txt": {Hash: "v1"}, "app.go": {Hash: "v1"}},
			host:           Manifest{"app.go": {Hash: "v2"}},
			guest:          Manifest{"doomed.txt": {Hash: "guest"}, "app.go": {Hash: "guest"}},
			wantUpdate:     []string{"app.go"},
			wantDelete:     []string{"doomed.txt"},
			wantOverridden: []string{"app.go", "doomed.txt"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			plan := Compute(tt.delivered, tt.host, tt.guest)
			assert.True(t, plan.Blocked(), "every row is a conflict before --force")
			forced := plan.Forced()
			assert.ElementsMatch(t, tt.wantUpdate, forced.Update)
			assert.ElementsMatch(t, tt.wantDelete, forced.Delete)
			assert.ElementsMatch(t, tt.wantOverridden, forced.Overridden())
			assert.False(t, forced.Blocked())
		})
	}
}
