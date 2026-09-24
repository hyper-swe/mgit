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
	pr, err := query(call, queryPR, vars)
	if err != nil {
		return nil, err
	}
	c := &collected{counts: map[string]int{}}
	if err := c.addTitle(pr); err != nil {
		return nil, err
	}
	if err := c.addText("description", "description", &pr.textNode); err != nil {
		return nil, err
	}
	if err := c.addComments(call, vars); err != nil {
		return nil, err
	}
	return c, c.addReviews(call, vars)
}

// pages runs a paginated query until its last page, handing each page to
// visit, which returns that page's pageInfo.
func pages(call api, q string, vars map[string]any, visit func(*prNode) (pageInfo, error)) error {
	var after any
	for i := 0; i < maxPages; i++ {
		pr, err := query(call, q, withAfter(vars, after))
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
	return pages(call, queryComments, vars, func(pr *prNode) (pageInfo, error) {
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
	return pages(call, queryReviews, vars, func(pr *prNode) (pageInfo, error) {
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

// gh runs gh with a request body on stdin and returns its stdout.
func gh(stdin []byte, args ...string) ([]byte, error) {
	cmd := exec.CommandContext(context.Background(), "gh", args...) //nolint:gosec // a fixed binary; the arguments are this program's own queries and repository path
	cmd.Stdin = bytes.NewReader(stdin)
	cmd.Stderr = os.Stderr
	return cmd.Output()
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
	all         bool
	l           lists
	hits        io.Writer
}

// checkOne checks one pull request and returns the exit code.
func checkOne(call api, o options, w io.Writer) int {
	got, err := fetchPR(call, o.owner, o.name, o.pr)
	if err != nil {
		fmt.Fprintf(w, "prtext: NOT CHECKED — #%d: %v\n", o.pr, err)
		return 2
	}
	hits := judge(got.versions, o.l, cutoff)
	if o.hits != nil {
		writeHits(o.hits, o.pr, hits)
	}
	return report(w, o.pr, got, hits, cutoff)
}

// sweep reports on every pull request and never gates: exit 2 when any
// could not be read, else 0.
func sweep(call api, numbers []int, o options, w io.Writer) int {
	var read, withHits, unread, gating int
	for _, n := range numbers {
		o.pr = n
		var buf bytes.Buffer
		code := checkOne(call, o, &buf)
		out := buf.String()
		switch {
		case code == 2:
			unread++
		case strings.Contains(out, "prtext: listed "):
			read++
			withHits++
		default:
			read++
		}
		if code == 1 {
			gating++
		}
		if code != 0 || strings.Contains(out, "prtext: listed ") {
			fmt.Fprint(w, out)
		}
	}
	fmt.Fprintf(w, "prtext: sweep (report only) — %d pull requests read, %d with a hit, %d with a hit at or after the cutoff, %d NOT CHECKED\n",
		read, withHits, gating, unread)
	if unread > 0 {
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

func main() {
	repo := flag.String("repo", os.Getenv("GITHUB_REPOSITORY"), "owner/name")
	pr := flag.Int("pr", 0, "the pull request to check")
	all := flag.Bool("all", false, "report on every pull request; never gates")
	terms := flag.String("terms", termsFile, "the terms digest list")
	names := flag.String("names", namesFile, "the names digest list")
	hitsOut := flag.String("hits-out", "", "append each hit with its matched digest to this file (triage only)")
	flag.Parse()
	os.Exit(run(*repo, *pr, *all, [3]string{*terms, *names, *hitsOut}))
}

func run(repo string, pr int, all bool, files [3]string) int {
	owner, name, ok := strings.Cut(repo, "/")
	if !ok || (pr == 0) == !all {
		fmt.Fprintln(os.Stderr, "prtext: -repo owner/name and exactly one of -pr N or -all are required")
		return 2
	}
	l, err := loadLists(files[0], files[1])
	if err != nil {
		fmt.Printf("prtext: NOT CHECKED — %v\n", err)
		return 2
	}
	o := options{owner: owner, name: name, pr: pr, all: all, l: l}
	if files[2] != "" {
		f, err := os.OpenFile(files[2], os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
		if err != nil {
			fmt.Printf("prtext: NOT CHECKED — %v\n", err)
			return 2
		}
		defer f.Close() //nolint:errcheck // triage file; a failed close loses triage lines, never a verdict
		o.hits = f
	}
	if !all {
		return checkOne(ghGraphQL, o, os.Stdout)
	}
	numbers, err := listPRs(repo)
	if err != nil {
		fmt.Printf("prtext: NOT CHECKED — could not list the pull requests: %v\n", err)
		return 2
	}
	return sweep(ghGraphQL, numbers, o, os.Stdout)
}
