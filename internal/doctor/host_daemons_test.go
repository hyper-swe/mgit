package doctor

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"

	"github.com/hyper-swe/mgit/internal/sandboxd/daemonrec"
)

// The rows are the host states MGIT-185/191 found — a healthy fleet, a
// daemon whose root is gone, a temp-root daemon eleven days old, a stale
// record — and a host whose runtime base cannot be read.
func TestHostDaemonsCheck(t *testing.T) {
	day := 24 * time.Hour
	tests := []struct {
		name       string
		daemons    []daemonrec.Listed
		runErr     error
		wantStatus Status
		wantIn     string
	}{
		{"five_repository_daemons_all_healthy", []daemonrec.Listed{
			{Record: daemonrec.Record{PID: 1, RepoRoot: "/w/a"}, Status: daemonrec.Status{Alive: true, Age: 2 * day}},
			{Record: daemonrec.Record{PID: 2, RepoRoot: "/w/b"}, Status: daemonrec.Status{Alive: true, Age: time.Hour}},
		}, nil, StatusOK, "2 daemon(s)"},
		{"a_root_that_vanished_is_a_leak", []daemonrec.Listed{
			{Record: daemonrec.Record{PID: 6445, RepoRoot: "/T/tmp.s0nX"}, Status: daemonrec.Status{Alive: true, RootGone: true, TempRoot: true, Age: 13 * day}},
		}, nil, StatusFailed, "6445"},
		{"a_temp_root_older_than_a_day_is_a_leak", []daemonrec.Listed{
			{Record: daemonrec.Record{PID: 43474, RepoRoot: "/T/tmp.G2xD"}, Status: daemonrec.Status{Alive: true, TempRoot: true, Age: 11 * day}},
		}, nil, StatusFailed, "/T/tmp.G2xD"},
		{"a_fresh_temp_root_is_an_e2e_run_not_a_leak", []daemonrec.Listed{
			{Record: daemonrec.Record{PID: 7, RepoRoot: "/T/tmp.fresh"}, Status: daemonrec.Status{Alive: true, TempRoot: true, Age: 5 * time.Minute}},
		}, nil, StatusOK, "1 daemon(s)"},
		{"a_dead_record_is_pruned_not_failed", []daemonrec.Listed{
			{Record: daemonrec.Record{PID: 9999, RepoRoot: "/w/a"}, Status: daemonrec.Status{Alive: false, Age: day}},
		}, nil, StatusOK, "0 daemon(s)"},
		{"no_runtime_base_is_NOT_a_pass", nil, errors.New("read runtime base: permission denied"), StatusNotChecked, "permission denied"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c := HostDaemonsCheck{List: func(context.Context) ([]daemonrec.Listed, error) { return tt.daemons, tt.runErr }}
			got := c.Run(context.Background())
			assert.Equal(t, tt.wantStatus, got.Status)
			assert.Contains(t, got.Summary+got.Reason, tt.wantIn)
			assert.Equal(t, "MGIT-191", got.Incident)
			assert.Equal(t, "daemons/host", got.Name)
			if tt.wantStatus == StatusFailed {
				assert.Contains(t, got.Remedy, "mgit sandbox daemons stop --repo-root", "the remedy must name the scoped stop, never a blanket pkill")
				assert.NotContains(t, got.Remedy, "pkill")
			}
		})
	}
}
