package main

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// `mgit log --task-id` is the REVIEWER'S view of what an agent did on a task,
// and it printed the index position instead of the commit subject:
//
//	02e9e757 [MGIT-111] pos=0
//	4de61056 [MGIT-111] pos=1
//
// The messages were correct and complete — `mgit show` printed them in full.
// The task-scoped log simply did not have them: it reads the SQLite index,
// whose columns are hash, content hash, agent, position, created_at. So the
// renderer fell back to the only distinguishing field it held.
//
// A trail of `pos=0 pos=1 pos=2` communicates nothing about the work, which
// makes the reviewer run `mgit show` once per commit — exactly the friction
// the task-scoped log exists to remove. It also undercuts the cadence label
// printed beneath it, which characterizes commits the reader cannot identify.
// Refs: MGIT-155, MGIT-110
func TestCommitSubject_FromTheCommitObject(t *testing.T) {
	tests := []struct {
		name string
		body string
		want string
	}{
		{
			name: "the_subject_is_the_first_line_without_the_task_marker",
			body: "[MGIT:MGIT-155] fix(log): show subjects\n\nA body that must not appear.\n",
			want: "fix(log): show subjects",
		},
		{
			name: "a_single_line_message",
			body: "wip",
			want: "wip",
		},
		{
			name: "leading_and_trailing_space_is_trimmed",
			body: "   spaced subject   \n\nbody\n",
			want: "spaced subject",
		},
		{
			name: "an_over_long_subject_is_bounded_for_one_line_output",
			body: strings.Repeat("x", 200),
			want: strings.Repeat("x", subjectWidth-1) + "…",
		},
		{
			name: "an_empty_message_yields_nothing_rather_than_a_fake_subject",
			body: "\n\n",
			want: "",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, commitSubject(tt.body))
		})
	}
}

// A commit whose object cannot be read degrades to a stated absence, never a
// blank that reads like an empty commit message. Refs: MGIT-155
func TestTaskLogLine_UnreadableObject_SaysSoRatherThanPrintingBlank(t *testing.T) {
	line := taskLogLine("02e9e757", "MGIT-155", "")
	assert.Contains(t, line, "02e9e757")
	assert.Contains(t, line, "MGIT-155")
	assert.Contains(t, line, "(message unavailable)",
		"a missing subject must be stated; a blank line reads as an empty commit message")
	assert.NotContains(t, line, "pos=", "the index position is not what a reviewer came for")
}

// The ordinary line carries the subject and drops the position. Refs: MGIT-155
func TestTaskLogLine_CarriesTheSubject(t *testing.T) {
	line := taskLogLine("02e9e757", "MGIT-155", "fix(log): show subjects")
	assert.Contains(t, line, "fix(log): show subjects")
	require.NotContains(t, line, "pos=")
}

// The task marker is stripped, because the log line already carries the task
// in its own column and printing it twice is noise in the column a reviewer
// scans. Found by reading real output after the unit tests were green — the
// tests asserted the subject was PRESENT, which it was, and said nothing about
// it being said twice. Refs: MGIT-155
func TestCommitSubject_StripsTheTaskMarkerTheLineAlreadyCarries(t *testing.T) {
	tests := []struct{ name, body, want string }{
		{name: "the_mgit_convention", body: "[MGIT:MGIT-155] fix(log): show subjects", want: "fix(log): show subjects"},
		{name: "no_marker_is_untouched", body: "fix(log): show subjects", want: "fix(log): show subjects"},
		{
			name: "a_bracketed_token_that_is_not_the_marker_survives",
			body: "[WIP] do not ship", want: "[WIP] do not ship",
		},
		{
			name: "a_malformed_marker_is_left_alone_rather_than_guessed_at",
			body: "[MGIT:unterminated", want: "[MGIT:unterminated",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, commitSubject(tt.body))
		})
	}
}
