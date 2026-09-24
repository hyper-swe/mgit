// prtext checks a pull request's text against hashed word lists. This
// repository is public, and a pull request's title, description, comments
// and reviews, and every earlier revision of each (an edit leaves the old
// text readable in the history), can carry words that must not appear on a
// public surface. The check reads all of it through the GraphQL API and
// compares SHA-256 digests of normalized candidates against two committed
// lists, so no listed word is ever written here in clear:
//
//   - terms.sha256, matched in the sibling repository's hashed-lint form
//     (runs of letters and digits, whitespace words stripped, adjacent runs
//     joined), so that repository's digests carry over unchanged;
//   - names.sha256, matched only as whole tokens.
//
// A hit names the field, the revision and its time, never the word. Text
// written at or after the cutoff gates (exit 1); earlier hits are reported
// and never gate. What cannot be read is NOT CHECKED (exit 2), never clean.
//
// Usage:
//
//	prtext -repo owner/name -pr N         check one pull request
//	prtext -repo owner/name -all          report on every pull request; never gates
//	prtext -board .mtix/tasks.json        report on every node of the board; never gates
//	       [-terms F] [-names F] [-hits-out F]
//
// -hits-out appends each hit with its matched digest to a file, for the
// maintainers' triage against their private word list; nothing else ever
// prints a digest. Refs: MGIT-242
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"regexp"
	"strconv"
	"strings"
)

const (
	termsFile = "scripts/prtext/terms.sha256"
	namesFile = "scripts/prtext/names.sha256"
	maxPages  = 50
)

var digestRE = regexp.MustCompile(`^[0-9a-f]{64}$`)

// loadList reads a digest list: one lowercase SHA-256 per line; blank lines
// and lines starting with # are skipped. Anything else is refused by line
// number, never echoed, since it may be a word in clear.
func loadList(path string) (map[string]bool, error) {
	raw, err := os.ReadFile(path) //nolint:gosec // G304: the list path is the operator's argument
	if err != nil {
		return nil, fmt.Errorf("read the list %s: %w", path, err)
	}
	set := map[string]bool{}
	for i, line := range strings.Split(string(raw), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		if !digestRE.MatchString(line) {
			return nil, fmt.Errorf("%s line %d is not a SHA-256 digest; a list carries digests only", path, i+1)
		}
		set[line] = true
	}
	return set, nil
}

func withAfter(vars map[string]any, after any) map[string]any {
	v := map[string]any{"after": after}
	for k, x := range vars {
		v[k] = x
	}
	return v
}

// fetchPR reads every piece of a pull request's text and its history.
func fetchPR(call api, owner, name string, number int) (*collected, error) {
	vars := map[string]any{"owner": owner, "name": name, "number": number}
	c := &collected{counts: map[string]int{}}
	pr, err := query(call, queryPR, vars, &c.budget)
	if err != nil {
		return nil, err
	}
	if err := c.addTitle(pr); err != nil {
		return nil, err
	}
	if err := c.addText("description", "description", &pr.textNode); err != nil {
		return nil, err
	}
	if pr.Comments.TotalCount > 0 {
		if err := c.addComments(call, vars); err != nil {
			return nil, err
		}
	}
	if pr.Reviews.TotalCount > 0 {
		if err := c.addReviews(call, vars); err != nil {
			return nil, err
		}
	}
	return c, nil
}

// pages runs a paginated query until its last page, handing each page to
// visit, which returns that page's pageInfo.
func (c *collected) pages(call api, q string, vars map[string]any, visit func(*prNode) (pageInfo, error)) error {
	var after any
	for i := 0; i < maxPages; i++ {
		pr, err := query(call, q, withAfter(vars, after), &c.budget)
		if err != nil {
			return err
		}
		info, err := visit(pr)
		if err != nil || !info.HasNextPage {
			return err
		}
		if info.EndCursor == "" {
			return errors.New("the API reported another page without a cursor to it")
		}
		after = info.EndCursor
	}
	return fmt.Errorf("more than %d pages; not checked", maxPages)
}

func (c *collected) addComments(call api, vars map[string]any) error {
	return c.pages(call, queryComments, vars, func(pr *prNode) (pageInfo, error) {
		for i := range pr.Comments.Nodes {
			n := &pr.Comments.Nodes[i]
			if err := c.addText("comments", fmt.Sprintf("comment %d", n.DatabaseID), n); err != nil {
				return pageInfo{}, err
			}
		}
		return pr.Comments.PageInfo, nil
	})
}

func (c *collected) addReviews(call api, vars map[string]any) error {
	return c.pages(call, queryReviews, vars, func(pr *prNode) (pageInfo, error) {
		for i := range pr.Reviews.Nodes {
			r := &pr.Reviews.Nodes[i]
			if err := c.addText("reviews", fmt.Sprintf("review %d", r.DatabaseID), &r.textNode); err != nil {
				return pageInfo{}, err
			}
			if r.Comments.PageInfo.HasNextPage {
				return pageInfo{}, fmt.Errorf("review %d: %w", r.DatabaseID, errMorePages)
			}
			for j := range r.Comments.Nodes {
				n := &r.Comments.Nodes[j]
				if err := c.addText("review comments", fmt.Sprintf("review comment %d", n.DatabaseID), n); err != nil {
					return pageInfo{}, err
				}
			}
		}
		return pr.Reviews.PageInfo, nil
	})
}

// gh runs gh with a request body on stdin and returns its stdout. A
// failure carries gh's own first line of complaint, so a rate limit reads
// as one.
func gh(stdin []byte, args ...string) ([]byte, error) {
	cmd := exec.CommandContext(context.Background(), "gh", args...) //nolint:gosec // a fixed binary; the arguments are this program's own queries and repository path
	cmd.Stdin = bytes.NewReader(stdin)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		first, _, _ := strings.Cut(strings.TrimSpace(stderr.String()), "\n")
		return nil, fmt.Errorf("%w: %s", err, first)
	}
	return out, nil
}

func ghGraphQL(q string, vars map[string]any) ([]byte, error) {
	body, err := json.Marshal(map[string]any{"query": q, "variables": vars})
	if err != nil {
		return nil, err
	}
	return gh(body, "api", "graphql", "--input", "-")
}

// options is one run's configuration.
type options struct {
	owner, name string
	pr          int
	l           lists
	hits        io.Writer
	floor       int // the sweep stops when the API budget falls below this
}

// checkOne checks one pull request and returns the exit code and the API
// budget its last query reported (nil when unknown).
func checkOne(call api, o options, w io.Writer) (int, *rateLimit) {
	got, err := fetchPR(call, o.owner, o.name, o.pr)
	if err != nil {
		fmt.Fprintf(w, "prtext: NOT CHECKED — #%d: %v\n", o.pr, err)
		return 2, nil
	}
	hits := judge(got.versions, o.l, cutoff)
	if o.hits != nil {
		writeHits(o.hits, o.pr, hits)
	}
	return report(w, o.pr, got, hits, cutoff), got.budget
}

// tally counts a sweep's outcomes.
type tally struct{ read, withHits, gating, unread int }

func (t *tally) add(code int, out string) {
	hit := carriesHit(out)
	switch {
	case code == 2:
		t.unread++
	case hit:
		t.read++
		t.withHits++
	default:
		t.read++
	}
	if code == 1 {
		t.gating++
	}
}

// sweep reports on every pull request and never gates. The API budget is
// shared with everything else on the account, so the sweep stops when the
// budget falls below the floor, or at a rate-limit refusal, and says where
// to resume; what it did not read is NOT CHECKED. Exit 2 when any pull
// request went unread, else 0.
func sweep(call api, numbers []int, o options, w io.Writer) int {
	var t tally
	for i, n := range numbers {
		o.pr = n
		var buf bytes.Buffer
		code, budget := checkOne(call, o, &buf)
		out := buf.String()
		t.add(code, out)
		if code != 0 || carriesHit(out) {
			fmt.Fprint(w, out)
		}
		limited := code == 2 && strings.Contains(strings.ToLower(out), "rate limit")
		if i+1 < len(numbers) && (limited || (budget != nil && budget.Remaining < o.floor)) {
			left := "unknown"
			if budget != nil {
				left = fmt.Sprintf("%d (resets %s)", budget.Remaining, budget.ResetAt)
			}
			t.unread += len(numbers) - i - 1
			fmt.Fprintf(w, "prtext: sweep stopped to spare the shared API budget: %s left, floor %d; resume with -from %d\n", left, o.floor, numbers[i+1])
			break
		}
	}
	fmt.Fprintf(w, "prtext: sweep (report only) — %d pull requests read, %d with a hit, %d with a hit at or after the cutoff, %d NOT CHECKED\n",
		t.read, t.withHits, t.gating, t.unread)
	if t.unread > 0 {
		return 2
	}
	return 0
}

func listPRs(repo string) ([]int, error) {
	raw, err := gh(nil, "api", "repos/"+repo+"/pulls?state=all&per_page=100", "--paginate", "--jq", ".[].number")
	if err != nil {
		return nil, err
	}
	var out []int
	for _, f := range strings.Fields(string(raw)) {
		n, err := strconv.Atoi(f)
		if err != nil {
			return nil, fmt.Errorf("a pull request number that is not a number: %w", err)
		}
		out = append(out, n)
	}
	return out, nil
}

func loadLists(terms, names string) (lists, error) {
	t, err := loadList(terms)
	if err != nil {
		return lists{}, err
	}
	n, err := loadList(names)
	if err != nil {
		return lists{}, err
	}
	if len(t) == 0 || len(n) == 0 {
		return lists{}, errors.New("an empty list checks nothing")
	}
	return lists{terms: t, names: n}, nil
}

// config is the command line.
type config struct {
	repo, terms, names, hitsOut, board string
	pr, from, floor                    int
	all                                bool
}

func main() {
	var c config
	flag.StringVar(&c.repo, "repo", os.Getenv("GITHUB_REPOSITORY"), "owner/name")
	flag.IntVar(&c.pr, "pr", 0, "the pull request to check")
	flag.BoolVar(&c.all, "all", false, "report on every pull request; never gates")
	flag.IntVar(&c.from, "from", 0, "with -all: start at this pull request number and go down (resume a stopped sweep)")
	flag.IntVar(&c.floor, "floor", 2500, "with -all: stop when the shared API budget falls below this")
	flag.StringVar(&c.terms, "terms", termsFile, "the terms digest list")
	flag.StringVar(&c.names, "names", namesFile, "the names digest list")
	flag.StringVar(&c.hitsOut, "hits-out", "", "append each hit with its matched digest to this file (triage only)")
	flag.StringVar(&c.board, "board", "", "report on every node of this board file instead of pull requests")
	flag.Parse()
	os.Exit(run(c))
}

func run(c config) int {
	owner, name, ok := strings.Cut(c.repo, "/")
	modes := 0
	for _, on := range []bool{c.pr != 0, c.all, c.board != ""} {
		if on {
			modes++
		}
	}
	if modes != 1 || !ok && c.board == "" {
		fmt.Fprintln(os.Stderr, "prtext: exactly one of -pr N, -all or -board F is required; -pr and -all need -repo owner/name")
		return 2
	}
	l, err := loadLists(c.terms, c.names)
	if err != nil {
		fmt.Printf("prtext: NOT CHECKED — %v\n", err)
		return 2
	}
	if c.board != "" {
		return checkBoard(c.board, l, os.Stdout)
	}
	o := options{owner: owner, name: name, pr: c.pr, l: l, floor: c.floor}
	if c.hitsOut != "" {
		f, err := os.OpenFile(c.hitsOut, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
		if err != nil {
			fmt.Printf("prtext: NOT CHECKED — %v\n", err)
			return 2
		}
		defer f.Close() //nolint:errcheck // triage file; a failed close loses triage lines, never a verdict
		o.hits = f
	}
	if !c.all {
		code, _ := checkOne(ghGraphQL, o, os.Stdout)
		return code
	}
	numbers, err := listPRs(c.repo)
	if err != nil {
		fmt.Printf("prtext: NOT CHECKED — could not list the pull requests: %v\n", err)
		return 2
	}
	return sweep(ghGraphQL, from(numbers, c.from), o, os.Stdout)
}

// from keeps the pull requests numbered at most n (all of them when n is 0).
func from(numbers []int, n int) []int {
	if n == 0 {
		return numbers
	}
	var out []int
	for _, x := range numbers {
		if x <= n {
			out = append(out, x)
		}
	}
	return out
}
