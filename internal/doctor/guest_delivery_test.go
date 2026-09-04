package doctor

import (
	"context"
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"

	"github.com/hyper-swe/mgit/internal/model"
)

// The rows are the answers a guest can give — agrees, disagrees on some
// paths, cannot be asked, nothing delivered yet, no sandbox — not anything
// derived from the check's own code.
func TestGuestDeliveryCheck(t *testing.T) {
	tests := []struct {
		name       string
		report     *model.GuestViewReport
		runErr     error
		wantStatus Status
		wantIn     string
	}{
		{
			name:       "the_guest_reads_every_delivered_path",
			report:     &model.GuestViewReport{Checked: 312},
			wantStatus: StatusOK,
			wantIn:     "312",
		},
		{
			name:       "the_guest_reads_old_bytes_is_the_MGIT_164_condition",
			report:     &model.GuestViewReport{Checked: 312, Stale: []string{"src/app.go (guest reads the old bytes)", "gone.go (guest cannot read it)"}},
			wantStatus: StatusFailed,
			wantIn:     "src/app.go",
		},
		{
			name:       "a_guest_that_cannot_be_asked_is_NOT_a_pass",
			report:     &model.GuestViewReport{Unverifiable: "the guest has no sha256sum"},
			wantStatus: StatusNotChecked,
			wantIn:     "sha256sum",
		},
		{
			name:       "nothing_delivered_yet_is_NOT_a_pass",
			report:     &model.GuestViewReport{Unverifiable: "nothing has been delivered to this sandbox yet"},
			wantStatus: StatusNotChecked,
			wantIn:     "nothing has been delivered",
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
			c := GuestDeliveryCheck{
				Probe: func(context.Context) (*model.GuestViewReport, error) { return tt.report, tt.runErr },
			}
			got := c.Run(context.Background())
			assert.Equal(t, tt.wantStatus, got.Status)
			assert.Contains(t, got.Summary+got.Reason, tt.wantIn)
			assert.Equal(t, "MGIT-164", got.Incident)
			assert.Equal(t, "guest/delivery", got.Name)
			if tt.wantStatus == StatusFailed {
				assert.Contains(t, got.Remedy, "mgit sandbox sync", "the remedy must name the verb that re-delivers")
				assert.Contains(t, got.Summary, "2 of 312", "the summary must say how much of the tree disagrees")
			}
		})
	}
}
