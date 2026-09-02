package doctor

import (
	"context"
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestNestedGitCheck(t *testing.T) {
	tests := []struct {
		name       string
		offenders  []string
		scanErr    error
		wantStatus Status
		wantIn     string
	}{
		{
			name:       "a_clean_tree_passes",
			wantStatus: StatusOK,
			wantIn:     "no nested repository",
		},
		{
			name:       "a_nested_git_is_caught_at_rest",
			offenders:  []string{"testdata/inner/.git"},
			wantStatus: StatusFailed,
			wantIn:     "testdata/inner/.git",
		},
		{
			name:       "several_are_counted_not_all_listed",
			offenders:  []string{"a/.git", "b/.git", "c/.git", "d/.git", "e/.git"},
			wantStatus: StatusFailed,
			wantIn:     "5",
		},
		{
			name:       "an_unreadable_store_is_not_a_pass",
			scanErr:    errors.New("index unreadable"),
			wantStatus: StatusNotChecked,
			wantIn:     "index unreadable",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c := NestedGitCheck{Scan: func() ([]string, error) { return tt.offenders, tt.scanErr }}
			got := c.Run(context.Background())

			assert.Equal(t, tt.wantStatus, got.Status)
			assert.Contains(t, got.Summary+got.Reason, tt.wantIn)
			assert.Equal(t, "MGIT-157", got.Incident, "a check must name the incident it converts")
			if tt.wantStatus == StatusFailed {
				assert.NotEmpty(t, got.Remedy, "a failure without a remedy has moved the mystery, not removed it")
				assert.Contains(t, got.Remedy, "mgit init")
			}
		})
	}
}

func TestGuestLocalhostCheck(t *testing.T) {
	tests := []struct {
		name       string
		output     string
		runErr     error
		wantStatus Status
		wantIn     string
	}{
		{
			name:       "the_guest_resolves_localhost",
			output:     "127.0.0.1\tlocalhost\n",
			wantStatus: StatusOK,
			wantIn:     "resolves localhost",
		},
		{
			name:       "no_hosts_entry_is_the_MGIT_159_condition",
			output:     "",
			wantStatus: StatusFailed,
			wantIn:     "cannot resolve localhost",
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
			c := GuestLocalhostCheck{
				Probe: func(context.Context) (string, error) { return tt.output, tt.runErr },
			}
			got := c.Run(context.Background())

			assert.Equal(t, tt.wantStatus, got.Status)
			assert.Contains(t, got.Summary+got.Reason, tt.wantIn)
			assert.Equal(t, "MGIT-159", got.Incident)
			if tt.wantStatus == StatusFailed {
				assert.Contains(t, got.Remedy, "sandbox base",
					"the remedy must name the command that rebuilds the base")
				assert.Contains(t, got.Summary, "DNS",
					"the summary must say WHY it matters, or the reader cannot judge urgency")
			}
		})
	}
}

// The framework's own contract: a check that could not run is never a failure,
// and never a pass either. Refs: MGIT-162
func TestFailed_NotCheckedIsNeitherPassNorFailure(t *testing.T) {
	results := make([]Result, 0, 3)
	results = append(results,
		Result{Name: "a", Status: StatusOK},
		Result{Name: "b", Status: StatusNotChecked, Reason: "no sandbox"})
	assert.False(t, Failed(results), "an un-runnable check must not fail the exit code")

	rendered := Render(results)
	assert.Contains(t, rendered, "why not: no sandbox")
	assert.Contains(t, rendered, "absence of evidence",
		"the report must say plainly that a skipped check is not a pass")

	results = append(results, Result{Name: "c", Status: StatusFailed, Remedy: "do the thing"})
	assert.True(t, Failed(results))
	assert.Contains(t, Render(results), "remedy: do the thing")
}

// Every registered check names its incident — the property that keeps the
// reason a check exists discoverable from its output. Refs: MGIT-162, R-H300
func TestEveryCheck_NamesItsIncident(t *testing.T) {
	for _, c := range []Check{
		NestedGitCheck{Scan: func() ([]string, error) { return nil, nil }},
		GuestLocalhostCheck{Probe: func(context.Context) (string, error) { return "127.0.0.1 localhost", nil }},
		ResponseCapCheck{Probe: answering()},
	} {
		got := c.Run(context.Background())
		require.NotEmpty(t, got.Incident, "%s does not name the incident it converts", c.Name())
		assert.Contains(t, got.Incident, "MGIT-")
	}
}

// The base-currency check: current passes, any mismatch fails with both
// versions named, and an unrecorded base FAILS rather than passing — because
// reporting silence as currency is the failure being fixed. Refs: MGIT-174
func TestBaseCurrencyCheck(t *testing.T) {
	tests := []struct {
		name       string
		composed   string
		running    string
		inspectErr error
		wantStatus Status
		wantIn     string
	}{
		{
			name: "composed_by_this_substrate", composed: "0.6.4", running: "0.6.4",
			wantStatus: StatusOK, wantIn: "0.6.4",
		},
		{
			name:     "composed_by_an_older_substrate_names_BOTH_versions",
			composed: "0.6.3", running: "0.6.4",
			wantStatus: StatusFailed, wantIn: "0.6.3",
		},
		{
			name:     "an_unrecorded_base_is_a_FAILURE_not_a_pass",
			composed: "", running: "0.6.4",
			wantStatus: StatusFailed, wantIn: "does not record",
		},
		{
			name:       "an_uninspectable_base_is_not_checked",
			inspectErr: errors.New("no guest base registered for this repository"),
			wantStatus: StatusNotChecked, wantIn: "no guest base registered",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c := BaseCurrencyCheck{Inspect: func() (string, string, error) {
				return tt.composed, tt.running, tt.inspectErr
			}}
			got := c.Run(context.Background())

			assert.Equal(t, tt.wantStatus, got.Status)
			assert.Contains(t, got.Summary+got.Reason, tt.wantIn)
			assert.Equal(t, "MGIT-174", got.Incident)
			if tt.wantStatus == StatusFailed {
				assert.Contains(t, got.Remedy, "sandbox base from")
			}
			if tt.composed == "0.6.3" {
				assert.Contains(t, got.Summary, "0.6.4",
					"a mismatch must name BOTH versions, or the reader cannot judge the gap")
				assert.Contains(t, got.Summary, "guest-side",
					"the consequence must be stated: a stale base lacks guest-side fixes")
			}
		})
	}
}
