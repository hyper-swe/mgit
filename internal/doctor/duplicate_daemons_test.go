package doctor

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/hyper-swe/mgit/internal/sandboxd/daemonrec"
)

// One repository served by two daemons — one per spelling of its path — is
// the condition under which registries diverge and a fresh daemon deletes a
// live sandbox (MGIT-197). The rows spell one repository two ways through a
// real symlink, so the check has to resolve paths, not compare bytes.
// Refs: MGIT-197, R-H300 rule 5
func TestDuplicateDaemonsCheck_TwoLiveDaemonsForOneRepository_Fails(t *testing.T) {
	base := t.TempDir()
	real := filepath.Join(base, "repo")
	require.NoError(t, os.MkdirAll(real, 0o750))
	link := filepath.Join(t.TempDir(), "link")
	require.NoError(t, os.Symlink(base, link))
	viaLink := filepath.Join(link, "repo")
	other := filepath.Join(base, "other")
	require.NoError(t, os.MkdirAll(other, 0o750))
	alive := daemonrec.Status{Alive: true}

	tests := []struct {
		name       string
		daemons    []daemonrec.Listed
		listErr    error
		wantStatus Status
		wantIn     []string
	}{
		{"two_repositories_two_daemons", []daemonrec.Listed{
			{Record: daemonrec.Record{PID: 1, RepoRoot: real, Socket: "/s/1"}, Status: alive},
			{Record: daemonrec.Record{PID: 2, RepoRoot: other, Socket: "/s/2"}, Status: alive},
		}, nil, StatusOK, []string{"2 daemon(s)"}},
		{"one_repository_two_spellings_two_daemons", []daemonrec.Listed{
			{Record: daemonrec.Record{PID: 1, RepoRoot: real, Socket: "/s/1"}, Status: alive},
			{Record: daemonrec.Record{PID: 2, RepoRoot: viaLink, Socket: "/s/2"}, Status: alive},
		}, nil, StatusFailed, []string{real, viaLink, "pid 1", "pid 2"}},
		{"a_dead_record_does_not_count", []daemonrec.Listed{
			{Record: daemonrec.Record{PID: 1, RepoRoot: real, Socket: "/s/1"}, Status: alive},
			{Record: daemonrec.Record{PID: 2, RepoRoot: viaLink, Socket: "/s/2"}, Status: daemonrec.Status{Alive: false}},
		}, nil, StatusOK, []string{"1 daemon(s)"}},
		{"unreadable_records_are_not_a_pass", nil, errors.New("read runtime base: permission denied"), StatusNotChecked, []string{"permission denied"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c := DuplicateDaemonsCheck{List: func(context.Context) ([]daemonrec.Listed, error) { return tt.daemons, tt.listErr }}
			got := c.Run(context.Background())
			assert.Equal(t, tt.wantStatus, got.Status)
			assert.Equal(t, "daemons/one-per-repository", got.Name)
			assert.Equal(t, "MGIT-197", got.Incident)
			for _, w := range tt.wantIn {
				assert.Contains(t, got.Summary+got.Reason, w)
			}
			if tt.wantStatus == StatusFailed {
				assert.Contains(t, got.Remedy, "mgit sandbox daemons stop --repo-root")
				assert.NotContains(t, got.Remedy, "pkill")
			}
		})
	}
}
