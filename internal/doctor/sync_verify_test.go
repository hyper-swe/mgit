package doctor

import (
	"context"
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"
)

// The rows are the shapes a guest can answer with — both tools, one, none,
// or no guest to ask — not anything read from the check's own code.
func TestGuestSyncVerifyCheck(t *testing.T) {
	tests := []struct {
		name       string
		output     string
		runErr     error
		wantStatus Status
		wantIn     string
	}{
		{
			name:       "the_guest_can_confirm_and_invalidate",
			output:     "sha256sum\ndrop_caches\n",
			wantStatus: StatusOK,
			wantIn:     "can confirm",
		},
		{
			name:       "no_invalidation_is_still_confirmable_and_says_so",
			output:     "sha256sum\n",
			wantStatus: StatusOK,
			wantIn:     "cannot be invalidated",
		},
		{
			name:       "no_sha256sum_is_the_MGIT_192_blind_spot",
			output:     "drop_caches\n",
			wantStatus: StatusFailed,
			wantIn:     "cannot confirm",
		},
		{
			name:       "no_sandbox_is_NOT_a_pass",
			runErr:     errors.New("no sandbox bound for this worktree"),
			wantStatus: StatusNotChecked,
			wantIn:     "no sandbox",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c := GuestSyncVerifyCheck{
				Probe: func(context.Context) (string, error) { return tt.output, tt.runErr },
			}
			got := c.Run(context.Background())
			assert.Equal(t, tt.wantStatus, got.Status)
			assert.Contains(t, got.Summary+got.Reason, tt.wantIn)
			assert.Equal(t, "MGIT-192", got.Incident)
			assert.Equal(t, "guest/sync-verify", got.Name)
			if tt.wantStatus == StatusFailed {
				assert.Contains(t, got.Remedy, "sha256sum",
					"the remedy must name what the guest image is missing")
				assert.Contains(t, got.Summary, "not verified",
					"the summary must say what every sync will report until this is fixed")
			}
		})
	}
}
