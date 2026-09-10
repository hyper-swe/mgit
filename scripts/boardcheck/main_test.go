package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

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
	assert.Contains(t, s, "boardcheck: DRIFT — 3 ticket(s) referenced by merged commits are still open on the board: MGIT-11.10.5, MGIT-204, MGIT-999")
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
	assert.Contains(t, out.String(), "DRIFT — 1 ticket(s)")
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
