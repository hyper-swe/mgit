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
//	  "$(git log -1 --format=%H -- .mtix/tasks.json)..HEAD" | boardcheck -board .mtix/tasks.json
//
// It is a REPORT: exit 0 whatever it finds, unless -strict asks for exit 1
// on unacknowledged drift; exit 2 when the board cannot be read (not
// checked is a verdict of its own, never a pass). Refs: MGIT-178, MGIT-187
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
		fmt.Fprintf(out, "boardcheck: NOT CHECKED — %v\n", err)
		return 2
	}
	commits := parseLog(log)
	if len(commits) == 0 {
		fmt.Fprintln(out, "boardcheck: no commits since the board's last update — nothing to compare")
		return 0
	}
	rows, drift := judge(commits, statuses)
	fmt.Fprintf(out, "boardcheck: %d commit(s) since the board's last update reference %d ticket(s)\n", len(commits), len(rows))
	for _, r := range rows {
		fmt.Fprintln(out, r)
	}
	if len(drift) == 0 {
		fmt.Fprintln(out, "boardcheck: no drift — every ticket referenced since the board's last update is closed or acknowledged")
		return 0
	}
	fmt.Fprintf(out, "boardcheck: DRIFT — %d ticket(s) referenced by merged commits are still open on the board: %s\n",
		len(drift), strings.Join(drift, ", "))
	if strict {
		return 1
	}
	return 0
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
// commits that reference nothing are dropped.
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
		if len(c.refs) > 0 {
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

// judge produces one row per referenced ticket and the sorted list of
// those that are unacknowledged drift.
func judge(commits []commit, statuses map[string]string) (rows []string, drift []string) {
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
			drift = append(drift, id)
		case closedStatuses[status]:
			rows = append(rows, fmt.Sprintf("  %s  %-11s  closed", id, status))
		case ack[id] != "" || hasAck(ack, id):
			rows = append(rows, fmt.Sprintf("  %s  %-11s  stays open (%s)", id, status, ack[id]))
		default:
			rows = append(rows, fmt.Sprintf("  %s  %-11s  still open  — %s %s", id, status, c.sha, c.subject))
			drift = append(drift, id)
		}
	}
	return rows, drift
}

func hasAck(ack map[string]string, id string) bool { _, ok := ack[id]; return ok }
