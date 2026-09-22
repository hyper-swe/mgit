// boardcheck reads the commits merged since the tracked board's last
// update and reports, for every ticket their Refs trailers name, what the
// board says about it. A ticket whose work has merged and which the board
// still lists as open is DRIFT: the board is the only shared view of what
// is left, and a board that lists shipped work as pending is wrong in the
// direction that wastes the most time (MGIT-178, the founder's ruling of
// 2026-08-24: a merge without a ticket close is an incomplete merge).
//
// The distinction the ruling insists on is kept: a merge closes a ticket
// only when its acceptance is met. A commit that must land while its ticket
// stays open says so in a trailer —
//
//	Stays-Open: MGIT-164 — root cause unreproduced; verification shipped
//
// and the report shows the reason beside the ticket instead of counting it.
//
// The tool runs no git itself: the workflow pipes it the log, so the
// contract is a text format anyone can reproduce by hand —
//
//	git log --no-merges \
//	  --format='%h%x09%s%x09%(trailers:key=Refs,valueonly,separator=%x2C)%x09%(trailers:key=Stays-Open,valueonly,separator=%x2C)' \
//	  "$(git log -1 --format=%H -- .mtix/tasks.json)^..HEAD" | boardcheck -board .mtix/tasks.json
//
// The window starts at the board commit's PARENT: the board commit is the
// boundary and its own trailers are read, so the export commit can carry the
// Stays-Open for a ticket it could not close instead of resetting that
// ticket's drift out of the report unacknowledged (MGIT-216).
//
// It is a REPORT: exit 0 whatever it finds, unless -strict asks for exit 1
// on unacknowledged drift; exit 2 when the board cannot be read (not
// checked is a verdict of its own, never a pass). Refs: MGIT-178, MGIT-187
//
// The last line of every run is `status: <description>` — the text the
// workflow writes as the commit status, bounded here to GitHub's 140
// characters. It counts the two drift conditions separately, lists the
// stricter one (not on the tracked board) first, and when ids do not fit
// says how many were left out instead of dropping them: the workflow used
// to cut the verdict line at 140, so a status once claimed nine tickets and
// named five. Refs: MGIT-211
package main

import (
	"bufio"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"regexp"
	"sort"
	"strings"
	"unicode/utf8"
)

// idRe matches a ticket id as the Refs convention writes it, dotted
// children included (MGIT-11.10.5). Other projects' ids are not this
// board's business.
var idRe = regexp.MustCompile(`^MGIT-[0-9]+(?:\.[0-9]+)*$`)

// closedStatuses are the board states under which a merged reference is
// not drift.
var closedStatuses = map[string]bool{"done": true, "cancelled": true} //nolint:misspell // OK: the board spells the status this way (15 nodes on it today)

type node struct {
	ID     string `json:"id"`
	Status string `json:"status"`
}

type commit struct {
	sha, subject string
	refs         []string
	staysOpen    map[string]string // ticket id → recorded reason
}

func main() {
	board := flag.String("board", ".mtix/tasks.json", "the tracked board of record")
	strict := flag.Bool("strict", false, "exit 1 on unacknowledged drift (default: report only)")
	flag.Parse()
	os.Exit(run(os.Stdin, os.Stdout, *board, *strict))
}

// run is main without the process: log in, report out, exit code back.
func run(log io.Reader, out io.Writer, boardPath string, strict bool) int {
	statuses, err := readBoard(boardPath)
	if err != nil {
		notChecked := fmt.Sprintf("boardcheck: NOT CHECKED — %v", err)
		fmt.Fprintln(out, notChecked)
		fmt.Fprintln(out, "status: "+fit(notChecked, statusLimit))
		return 2
	}
	commits := parseLog(log)
	if len(commits) == 0 {
		const none = "boardcheck: no commits since the board's last update — nothing to compare"
		fmt.Fprintln(out, none)
		fmt.Fprintln(out, "status: "+none)
		return 0
	}
	rows, notTracked, open := judge(commits, statuses)
	fmt.Fprintf(out, "boardcheck: %d commit(s) since the board's last update reference %d ticket(s)\n", len(commits), len(rows))
	for _, r := range rows {
		fmt.Fprintln(out, r)
	}
	if len(notTracked)+len(open) == 0 {
		const clean = "boardcheck: no drift — every ticket referenced since the board's last update is closed or acknowledged"
		fmt.Fprintln(out, clean)
		fmt.Fprintln(out, "status: "+clean)
		return 0
	}
	fmt.Fprintln(out, verdict(notTracked, open, 0))
	fmt.Fprintln(out, "status: "+verdict(notTracked, open, statusLimit))
	if strict {
		return 1
	}
	return 0
}

// statusLimit is GitHub's cap on a commit status description; a longer one
// is rejected outright. Refs: MGIT-211
const statusLimit = 140

// verdict renders the DRIFT line: both counts named, the not-tracked ids
// before the open ones (the stricter condition, where a short read still
// catches it), the groups separated by " | ". With limit > 0 the line is at
// most limit runes: ids that do not fit — open ones first — give way to a
// visible "… and N more — see the job summary" tail. Refs: MGIT-211
func verdict(notTracked, open []string, limit int) string {
	var counts []string
	if len(notTracked) > 0 {
		counts = append(counts, fmt.Sprintf("%d not on the tracked board", len(notTracked)))
	}
	if len(open) > 0 {
		counts = append(counts, fmt.Sprintf("%d open on the board", len(open)))
	}
	head := "boardcheck: DRIFT — " + strings.Join(counts, " + ") + ":"
	total := len(notTracked) + len(open)
	for shown := total; shown >= 0; shown-- {
		line := head + listIDs(notTracked, open, shown)
		if shown < total {
			line += fmt.Sprintf(" … and %d more — see the job summary", total-shown)
		}
		if limit == 0 || utf8.RuneCountInString(line) <= limit {
			return line
		}
	}
	return fit(head, limit) // only if the counts alone exceed the cap
}

// listIDs renders the first n ids, not-tracked ones first, " | " between
// the two groups when both are represented in what is shown.
func listIDs(notTracked, open []string, n int) string {
	var b strings.Builder
	for i, id := range notTracked {
		if i >= n {
			return b.String()
		}
		if i == 0 {
			b.WriteString(" ")
		} else {
			b.WriteString(", ")
		}
		b.WriteString(id)
	}
	for i, id := range open {
		if len(notTracked)+i >= n {
			break
		}
		switch {
		case i == 0 && len(notTracked) > 0:
			b.WriteString(" | ")
		case i == 0:
			b.WriteString(" ")
		default:
			b.WriteString(", ")
		}
		b.WriteString(id)
	}
	return b.String()
}

// fit bounds s to limit runes with a visible ellipsis (limit 0: unbounded).
func fit(s string, limit int) string {
	if limit == 0 || utf8.RuneCountInString(s) <= limit {
		return s
	}
	return string([]rune(s)[:limit-1]) + "…"
}

// readBoard maps every tracked ticket to its status.
func readBoard(path string) (map[string]string, error) {
	raw, err := os.ReadFile(path) //nolint:gosec // the board path the caller named
	if err != nil {
		return nil, fmt.Errorf("read the board: %w", err)
	}
	var b struct {
		Nodes []node `json:"nodes"`
	}
	if err := json.Unmarshal(raw, &b); err != nil {
		return nil, fmt.Errorf("parse the board: %w", err)
	}
	statuses := make(map[string]string, len(b.Nodes))
	for _, n := range b.Nodes {
		statuses[n.ID] = n.Status
	}
	return statuses, nil
}

// parseLog reads the four-field lines the workflow's git log prints;
// commits that name nothing — no Refs, no Stays-Open — are dropped.
func parseLog(r io.Reader) []commit {
	var commits []commit
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	for sc.Scan() {
		f := strings.Split(sc.Text(), "\t")
		if len(f) < 3 {
			continue
		}
		c := commit{sha: f[0], subject: f[1], refs: parseIDs(f[2]), staysOpen: map[string]string{}}
		if len(f) > 3 {
			c.staysOpen = parseStaysOpen(f[3])
		}
		if len(c.refs) > 0 || len(c.staysOpen) > 0 {
			commits = append(commits, c)
		}
	}
	return commits
}

// parseIDs splits a comma-joined Refs value into this board's ticket ids.
func parseIDs(s string) []string {
	var ids []string
	for _, p := range strings.Split(s, ",") {
		if id := strings.TrimSpace(p); idRe.MatchString(id) {
			ids = append(ids, id)
		}
	}
	return ids
}

// parseStaysOpen reads "MGIT-x — reason" values (comma-joined when a commit
// carries several).
func parseStaysOpen(s string) map[string]string {
	ack := map[string]string{}
	for _, p := range strings.Split(s, ",") {
		p = strings.TrimSpace(p)
		if p == "" {
			continue
		}
		id, reason, _ := strings.Cut(p, " ")
		reason = strings.TrimSpace(strings.TrimLeft(strings.TrimSpace(reason), "—-:"))
		if idRe.MatchString(id) {
			ack[id] = reason
		}
	}
	return ack
}

// judge produces one row per ticket the window names — by a Refs or by a
// Stays-Open — and, sorted, the two kinds of unacknowledged drift: tickets
// the tracked board does not carry at all (the stricter condition, judged
// before any acknowledgement) and tickets it still lists as open.
//
// A Stays-Open names its ticket even when no Refs in the window does: the
// commit that referenced the ticket may lie before the window (the board
// commit is the boundary), and the export commit is where the reason for
// not closing it is written — an acknowledgement is visible exactly when it
// is written, never dependent on what else the window holds. Refs: MGIT-216
func judge(commits []commit, statuses map[string]string) (rows, notTracked, open []string) {
	first := map[string]commit{}
	ack := map[string]string{}
	for _, c := range commits {
		for _, id := range c.refs {
			if _, seen := first[id]; !seen {
				first[id] = c
			}
		}
		for id, reason := range c.staysOpen {
			ack[id] = reason
			if _, seen := first[id]; !seen {
				first[id] = c
			}
		}
	}
	ids := make([]string, 0, len(first))
	for id := range first {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	for _, id := range ids {
		c := first[id]
		status, tracked := statuses[id]
		switch {
		case !tracked:
			rows = append(rows, fmt.Sprintf("  %s  (not on the tracked board)  — %s %s", id, c.sha, c.subject))
			notTracked = append(notTracked, id)
		case closedStatuses[status]:
			rows = append(rows, fmt.Sprintf("  %s  %-11s  closed", id, status))
		case ack[id] != "" || hasAck(ack, id):
			rows = append(rows, fmt.Sprintf("  %s  %-11s  stays open (%s)", id, status, ack[id]))
		default:
			rows = append(rows, fmt.Sprintf("  %s  %-11s  still open  — %s %s", id, status, c.sha, c.subject))
			open = append(open, id)
		}
	}
	return rows, notTracked, open
}

func hasAck(ack map[string]string, id string) bool { _, ok := ack[id]; return ok }
