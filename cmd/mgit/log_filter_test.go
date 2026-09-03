package main

import (
	"bytes"
	"io"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/hyper-swe/mgit/internal/model"
)

// logFixture is a trail with three commits by two agents, a day apart, so a
// filter's effect is unambiguous.
func logFixture() []*model.Commit {
	base := time.Date(2026, 8, 10, 12, 0, 0, 0, time.UTC)
	return []*model.Commit{
		{CommitID: strings.Repeat("a", 40), Message: "[MGIT:T-1] first\n\nbody", AgentID: "agent-a", CreatedAt: base},
		{CommitID: strings.Repeat("b", 40), Message: "second", AgentID: "agent-b", CreatedAt: base.AddDate(0, 0, 1)},
		{CommitID: strings.Repeat("c", 40), Message: "third", AgentID: "agent-a", CreatedAt: base.AddDate(0, 0, 2)},
	}
}

func idsOf(commits []*model.Commit) []string {
	out := make([]string, 0, len(commits))
	for _, c := range commits {
		out = append(out, c.CommitID[:1])
	}
	return out
}

const (
	day0 = "2026-08-10T12:00:00Z"
	day1 = "2026-08-11T12:00:00Z"
	day2 = "2026-08-12T12:00:00Z"
)

// filterCommits is the reviewer's narrowing of an audit trail, and it was at
// 10.5% coverage. Refs: FR-8.4
func TestFilterCommits(t *testing.T) {
	tests := []struct {
		name                 string
		since, until, author string
		want                 []string
		// wantErr rows are the MGIT-172 shapes: an unparseable bound is
		// REFUSED, naming the flag, never silently dropped.
		wantErr bool
	}{
		{name: "no_filter_returns_everything", want: []string{"a", "b", "c"}},
		{name: "since_is_inclusive_of_its_own_boundary", since: day1, want: []string{"b", "c"}},
		{name: "until_is_inclusive_of_its_own_boundary", until: day1, want: []string{"a", "b"}},
		{name: "since_and_until_bracket_one_commit", since: day1, until: day1, want: []string{"b"}},
		{name: "author_selects_that_agents_work", author: "agent-a", want: []string{"a", "c"}},
		{name: "an_unknown_author_selects_nothing", author: "nobody", want: []string{}},
		{name: "author_combines_with_a_window", since: day1, author: "agent-a", want: []string{"c"}},
		{
			name:  "a_window_that_excludes_everything_returns_nothing",
			since: day2, until: day0, want: []string{},
		},
		{
			name:    "a_plain_date_is_REFUSED_not_silently_ignored",
			since:   "2026-08-11",
			want:    []string{},
			wantErr: true,
		},
		{
			name:    "a_relative_word_is_REFUSED_not_silently_ignored",
			since:   "yesterday",
			want:    []string{},
			wantErr: true,
		},
		{
			name:    "an_unparseable_until_is_REFUSED_too",
			until:   "next tuesday",
			want:    []string{},
			wantErr: true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := filterCommits(logFixture(), tt.since, tt.until, tt.author)
			if tt.wantErr {
				require.Error(t, err, "an unparseable bound must be refused, not ignored")
				assert.Nil(t, got)
				return
			}
			require.NoError(t, err)
			assert.Equal(t, tt.want, idsOf(got))
		})
	}
}

// Written as a skipped probe naming MGIT-172; live since #90 fixed it.
//
// The property, stated so a fix cannot satisfy it by accident: the answer to a
// filter the tool could not apply must not be INDISTINGUISHABLE from the
// answer to no filter at all. That indistinguishability is the whole defect —
// same commits, same order, no warning, exit 0 — and it is what lets a
// reviewer believe a trail was narrowed when it was not. The fix answers with
// a refusal and nothing else. Refs: MGIT-172
func TestFilterCommits_AnUnappliedFilterIsNotSilentlyTheWholeTrail(t *testing.T) {
	all, err := filterCommits(logFixture(), "", "", "")
	require.NoError(t, err)
	rejected, err := filterCommits(logFixture(), "2026-08-11", "", "")

	require.Error(t, err, "a filter the tool could not apply must be refused")
	assert.Contains(t, err.Error(), "--since")
	assert.NotEqual(t, idsOf(all), idsOf(rejected),
		"a filter the tool could not apply must not answer exactly as if none were asked for")
}

// The regression control for MGIT-172's fix: a VALID window must keep working
// exactly as it does now. A fix that refused everything would satisfy the
// skipped tests above and destroy the feature.
func TestFilterCommits_AValidWindowKeepsWorking(t *testing.T) {
	got, err := filterCommits(logFixture(), day1, day2, "")
	require.NoError(t, err)

	assert.Equal(t, []string{"b", "c"}, idsOf(got))
}

// Filtering must not alias the caller's slice: the CLI applies --limit to the
// result afterwards, and a filter that returned a sub-slice of the input would
// let a later truncation reach back into it. Refs: FR-8.4
func TestFilterCommits_DoesNotAliasTheInput(t *testing.T) {
	in := logFixture()
	out, err := filterCommits(in, "", "", "agent-a")
	require.NoError(t, err)
	require.Len(t, out, 2)

	out[0] = &model.Commit{CommitID: "REPLACED"}

	assert.NotEqual(t, "REPLACED", in[0].CommitID, "the caller's slice must be untouched")
}

// firstLine is what the oneline renderer shows, and it had no coverage. A
// multi-line message must not spill its body into a one-line trail.
func TestFirstLine(t *testing.T) {
	tests := []struct{ name, in, want string }{
		{"a_single_line_is_itself", "just this", "just this"},
		{"a_body_is_dropped", "subject\n\nbody paragraph\nmore body", "subject"},
		{"an_empty_message_stays_empty", "", ""},
		{"a_leading_newline_yields_an_empty_first_line", "\nsubject", ""},
		{"crlf_leaves_no_stray_carriage_return_mid_line", "subject\r\nbody", "subject\r"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, firstLine(tt.in))
		})
	}
}

// renderLog's --limit is a bound on OUTPUT, not on the input it was handed:
// a log that printed more than asked would defeat the one flag a reviewer uses
// to keep a trail readable. Asserted by counting lines, not by matching them.
// Refs: FR-8.4
func TestRenderLog_LimitBoundsWhatIsPrinted(t *testing.T) {
	tests := []struct {
		name      string
		limit     int
		wantLines int
	}{
		{name: "a_limit_below_the_count_bounds_it", limit: 2, wantLines: 2},
		{name: "a_limit_above_the_count_prints_everything", limit: 99, wantLines: 3},
		{name: "a_limit_of_zero_is_unbounded", limit: 0, wantLines: 3},
		{name: "a_negative_limit_is_unbounded", limit: -1, wantLines: 3},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			out := captureStdout(t, func() {
				require.NoError(t, renderLog(logFixture(), tt.limit, true, false, ""))
			})
			assert.Equal(t, tt.wantLines, countLines(out), "output was:\n%s", out)
		})
	}
}

// Every format must identify each commit and say something about it. Asserted
// as a property over the formats rather than by pinning three layouts, so
// changing a separator does not fail this while dropping a field does.
// Refs: FR-8.4, MGIT-155
func TestRenderLog_EveryFormatIdentifiesEachCommitAndDescribesIt(t *testing.T) {
	commits := logFixture()
	for _, format := range []string{"", "oneline", "full"} {
		t.Run("format="+format, func(t *testing.T) {
			out := captureStdout(t, func() {
				require.NoError(t, renderLog(commits, 0, false, false, format))
			})
			for _, c := range commits {
				assert.Contains(t, out, c.ShortID(), "a reader must be able to name the commit")
				assert.Contains(t, out, firstLine(c.Message),
					"and must be told what it was — the MGIT-155 lesson")
			}
		})
	}
}

// --full is the format a reviewer reads one commit in, so it must carry the
// provenance the short forms leave out.
func TestRenderLog_FullFormatCarriesProvenance(t *testing.T) {
	commits := logFixture()
	out := captureStdout(t, func() {
		require.NoError(t, renderLog(commits, 1, false, false, "full"))
	})

	c := commits[0]
	assert.Contains(t, out, c.CommitID, "the full hash, not the abbreviation")
	assert.Contains(t, out, c.AgentID, "who did it")
	assert.Contains(t, out, c.CreatedAt.UTC().Format(time.RFC3339), "when, in UTC")
	assert.Contains(t, out, c.Message, "and the whole message, body included")
}

// --graph marks every line it prints and no others: a marker on some lines and
// not others would misread as structure that is not there.
func TestRenderLog_GraphMarksEveryLineItPrints(t *testing.T) {
	out := captureStdout(t, func() {
		require.NoError(t, renderLog(logFixture(), 2, true, true, ""))
	})

	lines := nonEmptyLines(out)
	require.Len(t, lines, 2)
	for _, l := range lines {
		assert.True(t, strings.HasPrefix(l, "* "), "every printed line carries the marker: %q", l)
	}
}

// An empty trail prints nothing at all rather than a header over no rows.
func TestRenderLog_AnEmptyTrailPrintsNothing(t *testing.T) {
	out := captureStdout(t, func() {
		require.NoError(t, renderLog(nil, 10, false, false, ""))
	})

	assert.Empty(t, out)
}

func countLines(s string) int { return len(nonEmptyLines(s)) }

func nonEmptyLines(s string) []string {
	var out []string
	for _, l := range strings.Split(s, "\n") {
		if strings.TrimSpace(l) != "" {
			out = append(out, l)
		}
	}
	return out
}

// captureStdout collects what a function writes to os.Stdout.
//
// renderLog writes to os.Stdout directly rather than to cmd.OutOrStdout(), so
// there is no writer to inject; capturing the file descriptor is the only way
// to observe it without changing product code.
func captureStdout(t *testing.T, fn func()) string {
	t.Helper()
	r, w, err := os.Pipe()
	require.NoError(t, err)
	orig := os.Stdout
	os.Stdout = w
	done := make(chan string, 1)
	go func() {
		var buf bytes.Buffer
		_, _ = io.Copy(&buf, r)
		done <- buf.String()
	}()

	fn()

	os.Stdout = orig
	require.NoError(t, w.Close())
	out := <-done
	require.NoError(t, r.Close())
	return out
}
