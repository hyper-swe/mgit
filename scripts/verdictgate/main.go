// verdictgate is the reviewer-verdict-at-head check: a pull request is
// green only when the latest reviewer verdict comment reads PASS and
// names the PR's CURRENT head sha. A moved head goes red until it is
// re-reviewed; a FAIL at head is red; no verdict at all is NOT CHECKED
// — red, never green. The check prints the sha it looked for and the
// one it found, so a reader never has to guess what it judged.
//
// The reviewer's comment format is the contract: the first line reads
//
//	VERDICT: PASS at <sha> …      or      VERDICT: FAIL at <sha> …
//
// where <sha> is a prefix (7+ hex) of the head it judged. The newest
// such comment wins; a verdict line anywhere but a comment's first
// line is a quotation, not a verdict. (MGIT-201; the mechanism the swe repository built first)
//
// Usage: verdictgate -repo owner/name -pr N   (reads GitHub through gh)
//
// Refs: MGIT-201
package main

import (
	"bufio"
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"regexp"
	"strings"
	"time"
)

// comment is one issue comment on the pull request.
type comment struct {
	Author  string
	Created string
	Body    string
}

// verdict is what the gate found: kind is "PASS", "FAIL" or "" when no
// verdict comment exists.
type verdict struct {
	Kind    string
	Sha     string
	Author  string
	Created string
}

var verdictLine = regexp.MustCompile(`^VERDICT:\s+(PASS|FAIL)\s+at\s+([0-9a-fA-F]{7,40})\b`)

// latestVerdict picks the newest verdict comment by its timestamp.
func latestVerdict(comments []comment) *verdict {
	var best *verdict
	var bestT time.Time
	for _, c := range comments {
		first := strings.TrimSpace(strings.SplitN(c.Body, "\n", 2)[0])
		m := verdictLine.FindStringSubmatch(first)
		if m == nil {
			continue
		}
		t, err := time.Parse(time.RFC3339, c.Created)
		if err != nil {
			t = time.Time{}
		}
		if best == nil || t.After(bestT) {
			best = &verdict{Kind: m[1], Sha: strings.ToLower(m[2]), Author: c.Author, Created: c.Created}
			bestT = t
		}
	}
	return best
}

// gate judges head against the comments and writes what it looked for
// and what it found; the LAST line it writes is the one-line verdict a
// commit status carries. It returns the exit code: 0 only for a PASS
// that names head.
func gate(head string, comments []comment, w io.Writer) int {
	head = strings.ToLower(head)
	fmt.Fprintf(w, "verdictgate: looked for %s (the pull request's current head)\n", head)
	// every session on this fleet posts under one login, so the gate
	// reads the verdict's FORMAT, not its author — a reviewer and an
	// author look the same here, and the words say so
	fmt.Fprintln(w, "verdictgate: reads the first line of each comment for 'VERDICT: PASS|FAIL at <sha>'; the newest wins; one login posts every comment here, so the author is not a signal")
	v := latestVerdict(comments)
	if v == nil {
		fmt.Fprintln(w, "verdictgate: NOT CHECKED — no verdict comment on this pull request (a comment whose first line reads 'VERDICT: PASS at <sha>' or 'VERDICT: FAIL at <sha>'); red until a reviewer posts one")
		return 1
	}
	fmt.Fprintf(w, "verdictgate: found %s at %s by %s (%s)\n", v.Kind, v.Sha, v.Author, v.Created)
	atHead := strings.HasPrefix(head, v.Sha)
	switch {
	case v.Kind == "PASS" && atHead:
		fmt.Fprintf(w, "verdictgate: PASS at %s names the current head — green\n", v.Sha)
		return 0
	case v.Kind == "PASS":
		fmt.Fprintf(w, "verdictgate: stale — the latest PASS names %s but the head is %s; re-review at the new head\n", v.Sha, head)
		return 1
	case atHead:
		fmt.Fprintf(w, "verdictgate: FAIL at %s is the latest verdict at the current head — red\n", v.Sha)
		return 1
	default:
		fmt.Fprintf(w, "verdictgate: the latest verdict is a FAIL at %s and the head has moved to %s — NOT CHECKED at this head; re-review\n", v.Sha, head)
		return 1
	}
}

// ghJSON runs gh api and returns its stdout.
func ghJSON(args ...string) ([]byte, error) {
	cmd := exec.CommandContext(context.Background(), "gh", append([]string{"api"}, args...)...) //nolint:gosec // a fixed binary; the arguments are this program's own repository path and jq filter, never user input
	cmd.Stderr = os.Stderr
	return cmd.Output()
}

func main() {
	repo := flag.String("repo", os.Getenv("GITHUB_REPOSITORY"), "owner/name")
	pr := flag.Int("pr", 0, "pull request number")
	flag.Parse()
	if *repo == "" || *pr == 0 {
		fmt.Fprintln(os.Stderr, "verdictgate: -repo owner/name and -pr N are required")
		os.Exit(2)
	}
	prPath := fmt.Sprintf("repos/%s/pulls/%d", *repo, *pr)
	headRaw, err := ghJSON(prPath, "--jq", ".head.sha")
	if err != nil {
		fmt.Fprintf(os.Stderr, "verdictgate: NOT CHECKED — could not read the pull request's head (%s): %v\n", prPath, err)
		os.Exit(2)
	}
	head := strings.TrimSpace(string(headRaw))
	raw, err := ghJSON(fmt.Sprintf("repos/%s/issues/%d/comments", *repo, *pr), "--paginate",
		"--jq", `.[] | {author: .user.login, created: .created_at, body: .body}`)
	if err != nil {
		fmt.Fprintf(os.Stderr, "verdictgate: NOT CHECKED — could not read the pull request's comments: %v\n", err)
		os.Exit(2)
	}
	var comments []comment
	sc := bufio.NewScanner(strings.NewReader(string(raw)))
	sc.Buffer(make([]byte, 1<<20), 1<<24)
	for sc.Scan() {
		var row struct {
			Author  string `json:"author"`
			Created string `json:"created"`
			Body    string `json:"body"`
		}
		if json.Unmarshal(sc.Bytes(), &row) == nil {
			comments = append(comments, comment{Author: row.Author, Created: row.Created, Body: row.Body})
		}
	}
	os.Exit(gate(head, comments, os.Stdout))
}
