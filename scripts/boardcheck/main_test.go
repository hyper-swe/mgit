package main

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

//nolint:misspell // OK: the board spells "cancelled" this way
const board = `{"nodes":[
 {"id":"MGIT-191","status":"done","title":"records"},
 {"id":"MGIT-204","status":"in_progress","title":"record before listen"},
 {"id":"MGIT-164","status":"open","title":"unreproduced root cause"},
 {"id":"MGIT-11.10.5","status":"open","title":"dotted id"},
 {"id":"MGIT-9","status":"cancelled","title":"the board spells it so"}
]}`

func boardFile(t *testing.T) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "tasks.json")
	require.NoError(t, os.WriteFile(p, []byte(board), 0o600))
	return p
}

// The log is what `git log --format='%h%x09%s%x09%(trailers:key=Refs,…)%x09%(trailers:key=Stays-Open,…)'`
// prints for the commits since the board's last update: one commit per
// line, four tab-separated fields, ids comma-joined inside the third.
func TestBoardcheck_ReportsEveryReferencedTicketByItsBoardStatus(t *testing.T) {
	log := strings.Join([]string{
		"87c81eb\tfix(sandbox): record before listen\tMGIT-204, MGIT-191\t",
		"3cf9e75\ttest(e2e): guest rows\tMGIT-11.10.5\t",
		"a1b2c3d\tchore: no ticket\t\t",
		"565e6ea\tfix: something\tMGIT-999\t",
	}, "\n") + "\n"
	var out bytes.Buffer
	code := run(strings.NewReader(log), &out, boardFile(t), false)
	s := out.String()
	assert.Equal(t, 0, code, "a report never fails the job unless asked (-strict)")
	assert.Contains(t, s, "MGIT-204  in_progress  still open")
	assert.Contains(t, s, "MGIT-191  done         closed")
	assert.Contains(t, s, "MGIT-11.10.5  open         still open", "dotted ids are tickets too")
	assert.Contains(t, s, "MGIT-999  (not on the tracked board)", "a ticket filed only in a local DB is drift of its own kind")
	assert.Contains(t, s, "boardcheck: DRIFT — 1 not on the tracked board + 2 open on the board: MGIT-999 | MGIT-11.10.5, MGIT-204",
		"the stricter condition is counted separately and listed first (MGIT-211)")
	assert.NotContains(t, s, "a1b2c3d", "a commit without a Refs trailer references nothing and is not listed")
}

// A merge closes a ticket only when its acceptance is met: a commit that
// says so in a Stays-Open trailer keeps the ticket out of the drift count,
// and the report shows the recorded reason beside it.
func TestBoardcheck_StaysOpenTrailerIsAcknowledgedNotDrift(t *testing.T) {
	log := "d7afea1\tfeat: daemon records\tMGIT-164, MGIT-191\tMGIT-164 — root cause unreproduced; verification shipped\n"
	var out bytes.Buffer
	code := run(strings.NewReader(log), &out, boardFile(t), true)
	s := out.String()
	assert.Equal(t, 0, code, "acknowledged drift is not drift, even under -strict")
	assert.Contains(t, s, "MGIT-164  open         stays open (root cause unreproduced; verification shipped)")
	assert.Contains(t, s, "boardcheck: no drift")
}

func TestBoardcheck_StrictExitsOneOnUnacknowledgedDrift(t *testing.T) {
	var out bytes.Buffer
	code := run(strings.NewReader("87c81eb\tfix\tMGIT-204\t\n"), &out, boardFile(t), true)
	assert.Equal(t, 1, code)
	assert.Contains(t, out.String(), "DRIFT — 1 open on the board")
}

func TestBoardcheck_NoCommitsSinceTheBoardIsSaidNotSilent(t *testing.T) {
	var out bytes.Buffer
	code := run(strings.NewReader(""), &out, boardFile(t), true)
	assert.Equal(t, 0, code)
	assert.Contains(t, out.String(), "boardcheck: no commits since the board's last update")
}

func TestBoardcheck_UnreadableBoardIsNotChecked(t *testing.T) {
	var out bytes.Buffer
	code := run(strings.NewReader("87c81eb\tfix\tMGIT-204\t\n"), &out, filepath.Join(t.TempDir(), "absent.json"), false)
	assert.Equal(t, 2, code, "a board that cannot be read is neither drift nor clean")
	assert.Contains(t, out.String(), "boardcheck: NOT CHECKED")
}

func TestParseIDs(t *testing.T) {
	assert.Equal(t, []string{"MGIT-204", "MGIT-191", "MGIT-11.10.5"}, parseIDs("MGIT-204, MGIT-191,MGIT-11.10.5, FEAT-2.28.4"))
	assert.Empty(t, parseIDs(""))
}

// statusLine is the one line the workflow turns into the commit status
// description; the tool prints it as `status: …`.
func statusLine(t *testing.T, s string) string {
	t.Helper()
	for _, l := range strings.Split(s, "\n") {
		if strings.HasPrefix(l, "status: ") {
			return strings.TrimPrefix(l, "status: ")
		}
	}
	t.Fatalf("no status: line in:\n%s", s)
	return ""
}

// GitHub caps a commit status description at 140 characters. The workflow
// used to `cut` the verdict line there, so on 182a915 a status that claimed
// nine tickets named five and nothing said a tail existed. The tool now
// prints a status line that fits by construction and says how many it could
// not fit; the summary's verdict line still names every one. Refs: MGIT-211
func TestBoardcheck_StatusLineFitsGitHubAndSaysWhatItDropped(t *testing.T) {
	var nodes, refs []string
	for i := 300; i < 330; i++ {
		nodes = append(nodes, fmt.Sprintf(`{"id":"MGIT-%d","status":"open","title":"t"}`, i))
		refs = append(refs, fmt.Sprintf("MGIT-%d", i))
	}
	p := filepath.Join(t.TempDir(), "tasks.json")
	require.NoError(t, os.WriteFile(p, []byte(`{"nodes":[`+strings.Join(nodes, ",")+`]}`), 0o600))
	var out bytes.Buffer
	run(strings.NewReader("abc1234\tfeat: thirty\t"+strings.Join(refs, ", ")+"\t\n"), &out, p, false)
	status := statusLine(t, out.String())
	assert.LessOrEqual(t, utf8.RuneCountInString(status), 140, "GitHub rejects a longer description: %q", status)
	assert.Contains(t, status, "30 open on the board")
	assert.Regexp(t, `… and [0-9]+ more — see the job summary$`, status, "a dropped tail is named, never silent")
	named := regexp.MustCompile(`MGIT-3[0-2][0-9]`).FindAllString(status, -1)
	more := regexp.MustCompile(`and ([0-9]+) more`).FindStringSubmatch(status)
	require.Len(t, more, 2)
	n, err := strconv.Atoi(more[1])
	require.NoError(t, err)
	assert.Equal(t, 30, len(named)+n, "the named ids and the tail count add up to every drifting ticket")
	var verdict string
	for _, l := range strings.Split(out.String(), "\n") {
		if strings.HasPrefix(l, "boardcheck: DRIFT") {
			verdict = l
		}
	}
	for _, id := range refs {
		assert.Contains(t, verdict, id, "the summary's verdict line is unbounded")
	}
}

// "Not on the tracked board" is judged before any acknowledgement — the
// stricter condition — so the status counts it separately and lists it
// first, where a short read still catches it. Refs: MGIT-211
func TestBoardcheck_StatusNamesNotTrackedFirstAndSeparately(t *testing.T) {
	var out bytes.Buffer
	run(strings.NewReader("87c81eb\tfix\tMGIT-204, MGIT-999\t\n"), &out, boardFile(t), false)
	assert.Equal(t, "boardcheck: DRIFT — 1 not on the tracked board + 1 open on the board: MGIT-999 | MGIT-204", statusLine(t, out.String()))
}

// Every verdict carries a status line, and none of them can exceed the
// cap — a NOT CHECKED reason is fitted with a visible ellipsis rather than
// handed to the workflow to cut. Refs: MGIT-211
func TestBoardcheck_EveryVerdictHasABoundedStatusLine(t *testing.T) {
	longDir := filepath.Join(t.TempDir(), strings.Repeat("a-very-long-directory-name-", 8))
	tests := []struct {
		name, log, board, want string
	}{
		{"no_drift", "d7afea1\tfeat\tMGIT-191\t\n", boardFile(t), "boardcheck: no drift"},
		{"no_commits", "", boardFile(t), "boardcheck: no commits"},
		{"not_checked_fitted", "87c81eb\tfix\tMGIT-204\t\n", filepath.Join(longDir, "absent.json"), "boardcheck: NOT CHECKED"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var out bytes.Buffer
			run(strings.NewReader(tt.log), &out, tt.board, false)
			status := statusLine(t, out.String())
			assert.True(t, strings.HasPrefix(status, tt.want), status)
			assert.LessOrEqual(t, utf8.RuneCountInString(status), 140, status)
			if tt.name == "not_checked_fitted" {
				assert.True(t, strings.HasSuffix(status, "…"), "a cut reason ends in a visible ellipsis: %q", status)
			}
		})
	}
}

// The trailer form the repository actually writes — the reason after an em
// dash on the same line — keeps its reason (a reviewer read it as dropped;
// run against git's own output for defe2b8 it is kept, and this pins it).
func TestParseStaysOpen_KeepsTheReasonAfterTheEmDash(t *testing.T) {
	ack := parseStaysOpen("MGIT-208 — closes on the publish and smoke receipts")
	assert.Equal(t, map[string]string{"MGIT-208": "closes on the publish and smoke receipts"}, ack)
	ack = parseStaysOpen("MGIT-164 - root cause unreproduced, MGIT-9 — ninety")
	assert.Equal(t, map[string]string{"MGIT-164": "root cause unreproduced", "MGIT-9": "ninety"}, ack, "a hyphen works too; commas separate tickets")
}

// An acknowledgement is visible exactly when it is written: a Stays-Open
// trailer for a ticket no windowed commit names in Refs still gets its row
// — the commit that named the ticket may lie before the window (the board
// commit is the boundary), and the export commit is where the reason for
// not closing it is recorded. Refs: MGIT-216
func TestBoardcheck_StaysOpenWithoutARefsInTheWindowIsShownNotSilent(t *testing.T) {
	log := "fbf8741\tchore(board): close MGIT-191 on its merge\tMGIT-191\tMGIT-164 — root cause unreproduced; verification shipped\n"
	var out bytes.Buffer
	code := run(strings.NewReader(log), &out, boardFile(t), true)
	s := out.String()
	assert.Equal(t, 0, code)
	assert.Contains(t, s, "MGIT-191  done         closed")
	assert.Contains(t, s, "MGIT-164  open         stays open (root cause unreproduced; verification shipped)",
		"the acknowledgement rides the commit that carries it, whether or not a Refs in the window names the ticket")
	assert.Contains(t, s, "boardcheck: no drift")
}

// A commit that carries only a Stays-Open trailer is a commit that names a
// ticket: it is read, not dropped as "references nothing".
func TestBoardcheck_StaysOpenOnlyCommitIsRead(t *testing.T) {
	log := "a1b2c3d\tdocs: what stays open and why\t\tMGIT-164 — the fix is a later delivery\n"
	var out bytes.Buffer
	code := run(strings.NewReader(log), &out, boardFile(t), true)
	s := out.String()
	assert.Equal(t, 0, code)
	assert.NotContains(t, s, "no commits since the board's last update", "a Stays-Open-only commit is not an empty window")
	assert.Contains(t, s, "MGIT-164  open         stays open (the fix is a later delivery)")
}

// The stricter condition holds for acknowledgements too: a Stays-Open that
// names a ticket the tracked board does not carry is drift of its own kind,
// whatever reason it gives.
func TestBoardcheck_StaysOpenForAnUntrackedTicketIsStillDrift(t *testing.T) {
	log := "a1b2c3d\tdocs: note\t\tMGIT-999 — filed locally only\n"
	var out bytes.Buffer
	code := run(strings.NewReader(log), &out, boardFile(t), true)
	s := out.String()
	assert.Equal(t, 1, code, "-strict: an untracked id is drift even when acknowledged")
	assert.Contains(t, s, "MGIT-999  (not on the tracked board)  — a1b2c3d docs: note")
	assert.Contains(t, s, "1 not on the tracked board")
}
