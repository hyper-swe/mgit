package worktreesync

import (
	"io/fs"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// A sync that did not deliver its plan must never report success.
//
// The field failure this guards: sync reported success while the guest tree
// lacked the created file, and mgit had no idea — it was caught by a
// consumer's stale-copy check ONE LAYER UP. A substrate that reports work it
// did not do sends an agent on to build against a tree missing its own
// changes, and every later result is derived from that.
//
// The root cause of that instance is still unreproduced. This check does not
// depend on knowing it: whatever drops an operation, the delivery is READ BACK
// and compared before anything is reported. Refs: MGIT-164
func TestVerifyDelivery(t *testing.T) {
	const (
		wantHash = "aaaa"
		oldHash  = "bbbb"
	)
	mode := fs.FileMode(0o644)

	tests := []struct {
		name     string
		plan     Plan
		intended Manifest // what the host staged
		actual   Manifest // what the guest tree holds after apply
		wantErr  string
	}{
		{
			name:     "a_delivered_update_verifies",
			plan:     Plan{Update: []string{"a.txt"}},
			intended: Manifest{"a.txt": {Hash: wantHash, Mode: mode}},
			actual:   Manifest{"a.txt": {Hash: wantHash, Mode: mode}},
		},
		{
			name:     "a_CREATION_that_never_landed_is_caught",
			plan:     Plan{Update: []string{"Created.test.tsx"}},
			intended: Manifest{"Created.test.tsx": {Hash: wantHash, Mode: mode}},
			actual:   Manifest{},
			wantErr:  "Created.test.tsx",
		},
		{
			name:     "a_stale_copy_is_caught_even_though_the_path_exists",
			plan:     Plan{Update: []string{"a.txt"}},
			intended: Manifest{"a.txt": {Hash: wantHash, Mode: mode}},
			actual:   Manifest{"a.txt": {Hash: oldHash, Mode: mode}},
			wantErr:  "a.txt",
		},
		{
			name:     "a_mode_that_did_not_land_is_caught_because_an_exec_bit_is_a_real_edit",
			plan:     Plan{Update: []string{"run.sh"}},
			intended: Manifest{"run.sh": {Hash: wantHash, Mode: 0o755}},
			actual:   Manifest{"run.sh": {Hash: wantHash, Mode: 0o644}},
			wantErr:  "run.sh",
		},
		{
			name:     "a_delete_that_did_not_happen_is_caught",
			plan:     Plan{Delete: []string{"gone.txt"}},
			intended: Manifest{},
			actual:   Manifest{"gone.txt": {Hash: oldHash, Mode: mode}},
			wantErr:  "gone.txt",
		},
		{
			name:     "a_completed_delete_verifies",
			plan:     Plan{Delete: []string{"gone.txt"}},
			intended: Manifest{},
			actual:   Manifest{},
		},
		{
			name: "several_missing_paths_are_all_named",
			plan: Plan{Update: []string{"a.txt", "b.txt"}},
			intended: Manifest{
				"a.txt": {Hash: wantHash, Mode: mode},
				"b.txt": {Hash: wantHash, Mode: mode},
			},
			actual:  Manifest{},
			wantErr: "b.txt",
		},
		{
			name:     "paths_outside_the_plan_are_not_policed",
			plan:     Plan{Update: []string{"a.txt"}},
			intended: Manifest{"a.txt": {Hash: wantHash, Mode: mode}},
			actual: Manifest{
				"a.txt":           {Hash: wantHash, Mode: mode},
				"guest-made-this": {Hash: oldHash, Mode: mode},
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := VerifyDelivery(tt.plan, tt.intended, tt.actual)
			if tt.wantErr == "" {
				require.NoError(t, err)
				return
			}
			require.Error(t, err, "an undelivered plan must never verify")
			assert.Contains(t, err.Error(), tt.wantErr, "the failure must NAME what did not land")
			assert.Contains(t, strings.ToLower(err.Error()), "guest",
				"the message must say the guest cannot read it, which is the fact that matters")
		})
	}
}
