package main

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/hyper-swe/mgit/internal/model"
)

// logTrail is three commits a day apart, by two agents.
func logTrail() []*model.Commit {
	day := func(n int) time.Time { return time.Date(2026, 8, 10+n, 12, 0, 0, 0, time.UTC) }
	return []*model.Commit{
		{CommitID: "a", AgentID: "agent-a", CreatedAt: day(0)},
		{CommitID: "b", AgentID: "agent-b", CreatedAt: day(1)},
		{CommitID: "c", AgentID: "agent-a", CreatedAt: day(2)},
	}
}

func trailIDs(commits []*model.Commit) []string {
	out := make([]string, 0, len(commits))
	for _, c := range commits {
		out = append(out, c.CommitID)
	}
	return out
}

// A --since or --until the tool cannot parse must FAIL the command, naming
// the flag, the value and an acceptable form — never return the unfiltered
// trail. `mgit log` is the reviewer's view of the audit trail, and the output
// of a rejected filter is indistinguishable from a filter that matched
// everything: same commits, same order, exit 0. The rows are the inputs a
// person actually types (a plain date, a relative word), not exotic ones.
// Refs: MGIT-172, MGIT-160
func TestFilterCommits_AnUnparseableWindowIsRefusedNamingTheFlag(t *testing.T) {
	tests := []struct {
		name         string
		since, until string
		author       string
		wantIDs      []string
		wantErr      []string
	}{
		{name: "no_window_no_filter", wantIDs: []string{"a", "b", "c"}},
		{name: "a_valid_since_filters", since: "2026-08-11T00:00:00Z", wantIDs: []string{"b", "c"}},
		{name: "a_valid_until_filters", until: "2026-08-11T23:59:59Z", wantIDs: []string{"a", "b"}},
		{name: "a_valid_window_with_an_offset_filters", since: "2026-08-11T02:00:00+02:00", wantIDs: []string{"b", "c"}},
		{name: "author_is_unchanged", author: "agent-a", wantIDs: []string{"a", "c"}},
		{name: "a_plain_date_since_is_refused", since: "2026-08-15", wantErr: []string{"--since", "2026-08-15", "2006-01-02T15:04:05Z"}},
		{name: "a_relative_word_since_is_refused", since: "yesterday", wantErr: []string{"--since", "yesterday"}},
		{name: "a_plain_date_until_is_refused", until: "2026-08-15", wantErr: []string{"--until", "2026-08-15"}},
		{name: "a_relative_phrase_until_is_refused", until: "last week", wantErr: []string{"--until", "last week"}},
		{name: "a_bad_until_is_refused_even_with_a_good_since", since: "2026-08-11T00:00:00Z", until: "next tuesday", wantErr: []string{"--until", "next tuesday"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := filterCommits(logTrail(), tt.since, tt.until, tt.author)
			if len(tt.wantErr) > 0 {
				require.Error(t, err, "an unparseable window must refuse, not answer")
				for _, want := range tt.wantErr {
					assert.Contains(t, err.Error(), want)
				}
				assert.Nil(t, got, "nothing is returned beside a refusal")
				return
			}
			require.NoError(t, err)
			assert.Equal(t, tt.wantIDs, trailIDs(got))
		})
	}
}

// The command itself fails, with exit status and the flag named — not a
// quiet full trail. Refs: MGIT-172
func TestLogCmd_AnUnparseableSince_FailsTheCommand(t *testing.T) {
	repo := t.TempDir()
	t.Chdir(repo)
	require.NoError(t, runCLI(t, "init"))
	require.NoError(t, runCLI(t, "commit", "-a", "--task-id", "MGIT-172", "-m", "seed", "--allow-empty"))

	err := runCLI(t, "log", "--since", "yesterday")

	require.Error(t, err, "a --since the tool cannot apply must fail the command")
	assert.Contains(t, err.Error(), "--since")
	assert.Contains(t, err.Error(), "yesterday")

	out, err := runCLIOut(t, "log", "--since", "2000-01-01T00:00:00Z", "--oneline")
	require.NoError(t, err, "a valid window still works")
	assert.Contains(t, out, "seed")
}
