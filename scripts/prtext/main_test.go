package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// The listed words in these tests are made up, so no word from the real
// lists is ever written into this repository: "marmalade" stands for a
// term, "quokka-zz-17" for a name. Refs: MGIT-242
const (
	testTerm = "marmalade"
	testName = "quokka-zz-17"
)

func testLists() lists {
	return lists{
		terms: map[string]bool{digest(testTerm): true, digest("tealeaf"): true},
		names: map[string]bool{digest(normalize(testName)): true},
	}
}

// fakeAPI answers each query from a fixture, the way the GraphQL API would.
func fakeAPI(t *testing.T, calls *[]string) api {
	t.Helper()
	read := func(name string) []byte {
		raw, err := os.ReadFile(filepath.Join("testdata", name)) //nolint:gosec // G304: test fixtures named in this file
		require.NoError(t, err)
		return raw
	}
	return func(query string, vars map[string]any) ([]byte, error) {
		*calls = append(*calls, fmt.Sprintf("%v", vars["after"]))
		switch query {
		case queryPR:
			return read("pr.json"), nil
		case queryComments:
			if vars["after"] == "c1" {
				return read("comments-2.json"), nil
			}
			return read("comments-1.json"), nil
		case queryReviews:
			return read("reviews.json"), nil
		}
		return nil, errors.New("unexpected query")
	}
}

func checkFixture(t *testing.T) (int, string) {
	t.Helper()
	var calls []string
	got, err := fetchPR(fakeAPI(t, &calls), "owner", "repo", 7)
	require.NoError(t, err)
	var out bytes.Buffer
	code := report(&out, 7, got, judge(got.versions, testLists(), cutoff), cutoff)
	return code, out.String()
}

// RED FIRST (MGIT-242): a listed word in a renamed title, in an old edit
// revision of the description and in a review comment is each found and
// gates; the same word in a comment written before the cutoff is reported
// and does not gate; near misses are not hits.
func TestCheck_FindsEveryPlaceAListedWordCanHide(t *testing.T) {
	code, out := checkFixture(t)

	assert.Equal(t, 1, code, "a listed word at or after the cutoff fails the check:\n%s", out)
	for _, want := range []string{
		"listed name in title as opened 2026-09-24T11:00:00Z — gating",
		"listed term in description revision 2026-09-24T11:00:00Z — gating",
		"listed term in review comment 301 as written 2026-09-24T11:40:00Z — gating",
		"listed name in comment 101 as written 2026-09-20T00:00:00Z — reported, not gating",
		"1 revision deleted from the history",
	} {
		assert.Contains(t, out, want)
	}
	assert.NotContains(t, out, "comment 102", "near misses are not hits: a longer token, a token inside a longer one, the name split by spaces, a longer run")
	assert.Equal(t, 4, strings.Count(out, "prtext: listed "), "exactly the four hits:\n%s", out)
	last := lastLine(out)
	assert.True(t, strings.HasPrefix(last, "prtext: FAIL — 3 hits (a listed word or a private address) at or after"), last)
}

// A HIT NAMES THE PLACE, NEVER THE WORD. Neither the listed words nor any
// digest of them, nor any fragment of the text that carried them, reaches
// the output: a public log is a public surface. Refs: MGIT-242
func TestCheck_OutputNeverCarriesTheWord(t *testing.T) {
	_, out := checkFixture(t)
	low := strings.ToLower(out)
	for _, secret := range []string{testTerm, "quokka", "zz-17", "zz17", digest(testTerm)[:12], digest(normalize(testName))[:12]} {
		assert.NotContains(t, low, secret)
	}
}

func TestCheck_ReadsEveryPageOfComments(t *testing.T) {
	var calls []string
	got, err := fetchPR(fakeAPI(t, &calls), "owner", "repo", 7)
	require.NoError(t, err)
	assert.Contains(t, calls, "c1", "the second page was asked for by its cursor")
	assert.Equal(t, 2, got.counts["comments"], "both pages' comments were read")
}

// A PAGE THE CHECK DID NOT READ IS NOT CHECKED, never clean: a nested
// connection with more than one page, an API error, or a GraphQL error
// is exit 2. Refs: MGIT-242
func TestFetch_WhatItCannotReadIsNotChecked(t *testing.T) {
	overflow := func(query string, _ map[string]any) ([]byte, error) {
		if query == queryPR {
			return []byte(`{"data":{"repository":{"pullRequest":{"createdAt":"2026-09-24T11:00:00Z","title":"t","body":"b",` +
				`"userContentEdits":{"pageInfo":{"hasNextPage":true},"nodes":[]},"timelineItems":{"pageInfo":{"hasNextPage":false},"nodes":[]}}}}}`), nil
		}
		return []byte(`{"data":{"repository":{"pullRequest":{"comments":{"nodes":[]},"reviews":{"nodes":[]}}}}}`), nil
	}
	tests := []struct {
		name, want string
		call       api
	}{
		{"more edit revisions than one page", "more than one page", overflow},
		{"the API refuses", "boom", func(string, map[string]any) ([]byte, error) { return nil, errors.New("boom") }},
		{"a GraphQL error", "Could not resolve", func(string, map[string]any) ([]byte, error) {
			return []byte(`{"errors":[{"message":"Could not resolve to a PullRequest"}]}`), nil
		}},
		{"no pull request", "no pull request", func(string, map[string]any) ([]byte, error) {
			return []byte(`{"data":{"repository":{"pullRequest":null}}}`), nil
		}},
		{"more review comments than one page", "review 201", answers(
			`"comments":{"pageInfo":{"hasNextPage":false},"nodes":[]}`,
			`"reviews":{"pageInfo":{"hasNextPage":false},"nodes":[{"databaseId":201,"createdAt":"2026-09-24T11:00:00Z","body":"",`+
				`"userContentEdits":{"pageInfo":{"hasNextPage":false},"nodes":[]},"comments":{"pageInfo":{"hasNextPage":true},"nodes":[]}}]}`)},
		{"another page with no cursor to it", "without a cursor", answers(
			`"comments":{"pageInfo":{"hasNextPage":true,"endCursor":""},"nodes":[]}`,
			`"reviews":{"pageInfo":{"hasNextPage":false},"nodes":[]}`)},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := fetchPR(tt.call, "owner", "repo", 7)
			require.Error(t, err)
			assert.Contains(t, err.Error(), tt.want)
		})
	}
}

// The cutoff is inclusive, and a version whose time cannot be read gates:
// not knowing when text was written is not evidence it came before.
func TestJudge_GatesAtOrAfterTheCutoffAndWhenTheTimeIsUnknown(t *testing.T) {
	text := "carries " + testTerm
	tests := []struct {
		name   string
		at     time.Time
		gating bool
	}{
		{"a second before", cutoff.Add(-time.Second), false},
		{"exactly at", cutoff, true},
		{"after", cutoff.Add(time.Hour), true},
		{"time unreadable", time.Time{}, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			hits := judge([]version{{Field: "description", Rev: "as written", At: tt.at, Text: text}}, testLists(), cutoff)
			require.Len(t, hits, 1)
			assert.Equal(t, tt.gating, hits[0].Gating)
		})
	}
}

// Terms use the sibling repository's hashed-lint form, so its digests carry
// over unchanged: each run of letters and digits, each whitespace word with
// the rest stripped, each pair of adjacent runs; nothing under 4 characters.
// Names match only as whole tokens. Refs: MGIT-242
func TestCandidates_TermsAndNamesUseTheirOwnForms(t *testing.T) {
	terms := termCandidates("A Tea leaf, tea-leaf; ab-cd x")
	for _, want := range []string{"leaf", "tealeaf", "abcd"} {
		assert.True(t, terms[want], "term candidates carry %q: %v", want, terms)
	}
	assert.False(t, terms["tea"], "shorter than 4 characters is never a candidate")

	tests := []struct {
		text string
		hit  bool
	}{
		{"by quokka-zz-17", true},
		{"(`quokka-zz-17`),", true},
		{"quokka-zz-17's review", true},
		{"QUOKKA_ZZ_17", true},
		{"quokka-zz-170", false},
		{"x-quokka-zz-17", false},
		{"quokka zz 17", false},
	}
	for _, tt := range tests {
		t.Run(tt.text, func(t *testing.T) {
			assert.Equal(t, tt.hit, anyListed(nameCandidates(tt.text), testLists().names) != "")
		})
	}
}

// THE COMMITTED LISTS CARRY DIGESTS ONLY. A word in clear in a list would
// publish the very thing the list keeps off this repository.
func TestTheCommittedListsCarryDigestsOnly(t *testing.T) {
	for _, name := range []string{termsFile, namesFile} {
		t.Run(name, func(t *testing.T) {
			set, err := loadList(filepath.Join("..", "..", name))
			require.NoError(t, err)
			assert.NotEmpty(t, set, "an empty list checks nothing")
		})
	}
	dir := t.TempDir()
	clear := filepath.Join(dir, "bad.sha256")
	require.NoError(t, os.WriteFile(clear, []byte("# a comment\n\n"+digest("x")+"\nnot-a-digest\n"), 0o600))
	_, err := loadList(clear)
	require.Error(t, err, "a line that is not a digest is refused")
	assert.NotContains(t, err.Error(), "not-a-digest", "the refusal names the line number, not its content")
}

// The sweep mode's triage file records which digest matched, for the
// maintainers to classify hits against their private word list; it is
// written only when asked for and never to the output. Refs: MGIT-242
func TestHitsFile_RecordsTheMatchedDigestOutsideTheOutput(t *testing.T) {
	var calls []string
	got, err := fetchPR(fakeAPI(t, &calls), "owner", "repo", 7)
	require.NoError(t, err)
	var buf bytes.Buffer
	writeHits(&buf, 7, judge(got.versions, testLists(), cutoff))
	lines := strings.Split(strings.TrimSpace(buf.String()), "\n")
	require.Len(t, lines, 4)
	assert.Regexp(t, regexp.MustCompile(`^7\ttitle\tas opened\t2026-09-24T11:00:00Z\tname\tgating\t[0-9a-f]{64}$`), lines[0])
	_, err = json.Marshal(lines)
	require.NoError(t, err)
}

// answers is an API whose pull request is clean and whose comments and
// reviews pages are the given JSON members.
func answers(comments, reviews string) api {
	return func(query string, _ map[string]any) ([]byte, error) {
		body := comments
		switch query {
		case queryPR:
			body = `"createdAt":"2026-09-24T11:00:00Z","title":"t","body":"b","comments":{"totalCount":1},"reviews":{"totalCount":1},` +
				`"userContentEdits":{"pageInfo":{"hasNextPage":false},"nodes":[]},"timelineItems":{"pageInfo":{"hasNextPage":false},"nodes":[]}`
		case queryReviews:
			body = reviews
		}
		return []byte(`{"data":{"repository":{"pullRequest":{` + body + `}}}}`), nil
	}
}

// A pull request with no comments and no reviews costs one query: the
// counts in the first answer spare the two that would return nothing.
func TestFetch_SendsNoQueryThatWouldReturnNothing(t *testing.T) {
	var sent []string
	quiet := func(query string, _ map[string]any) ([]byte, error) {
		sent = append(sent, query)
		return []byte(`{"data":{"repository":{"pullRequest":{"createdAt":"2026-09-24T11:00:00Z","title":"t","body":"b",` +
			`"comments":{"totalCount":0},"reviews":{"totalCount":0},"userContentEdits":{"pageInfo":{"hasNextPage":false},"nodes":[]},` +
			`"timelineItems":{"pageInfo":{"hasNextPage":false},"nodes":[]}}}}}`), nil
	}
	_, err := fetchPR(quiet, "owner", "repo", 7)
	require.NoError(t, err)
	assert.Equal(t, []string{queryPR}, sent)
}

// THE API BUDGET IS SHARED. A sweep stops when the budget an answer reports
// falls below its floor, or at a rate-limit refusal, names where to resume,
// and counts what it did not read as NOT CHECKED. Refs: MGIT-242
func TestSweep_StopsToSpareTheSharedBudget(t *testing.T) {
	low := func(query string, _ map[string]any) ([]byte, error) {
		return []byte(`{"data":{"rateLimit":{"remaining":100,"resetAt":"2026-09-24T11:40:00Z"},"repository":{"pullRequest":{` +
			`"createdAt":"2026-09-24T11:00:00Z","title":"t","body":"b","comments":{"totalCount":0},"reviews":{"totalCount":0},` +
			`"userContentEdits":{"pageInfo":{"hasNextPage":false},"nodes":[]},"timelineItems":{"pageInfo":{"hasNextPage":false},"nodes":[]}}}}}`), nil
	}
	calls := 0
	refused := func(string, map[string]any) ([]byte, error) {
		calls++
		return nil, errors.New("exit status 1: gh: API rate limit already exceeded for user ID 1")
	}
	tests := []struct {
		name, want, unread string
		call               api
	}{
		{"below the floor", "100 (resets 2026-09-24T11:40:00Z) left, floor 2500; resume with -from 8", "2 NOT CHECKED", low},
		{"refused by the rate limit", "unknown left, floor 2500; resume with -from 8", "3 NOT CHECKED", refused},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var out bytes.Buffer
			code := sweep(tt.call, []int{9, 8, 7}, options{owner: "o", name: "r", l: testLists(), floor: 2500}, &out)
			assert.Equal(t, 2, code, "what the sweep did not read is not checked")
			assert.Contains(t, out.String(), tt.want)
			assert.Contains(t, lastLine(out.String()), tt.unread)
		})
	}
	assert.Equal(t, 1, calls, "after a rate-limit refusal the sweep sends nothing more")
	assert.Equal(t, []int{8, 7}, from([]int{9, 8, 7}, 8))
}

func lastLine(s string) string {
	lines := strings.Split(strings.TrimSpace(s), "\n")
	return lines[len(lines)-1]
}

// GraphQL refuses a query that declares a variable it never uses, and the
// fixtures above never reach a real validator: the first live run of this
// check went NOT CHECKED on exactly that. Every declared variable must be
// used in the query's body. Refs: MGIT-242
func TestQueries_UseEveryVariableTheyDeclare(t *testing.T) {
	declared := regexp.MustCompile(`\$(\w+):`)
	for name, q := range map[string]string{"pr": queryPR, "comments": queryComments, "reviews": queryReviews} {
		head, body, ok := strings.Cut(q, "){")
		require.True(t, ok, name)
		for _, m := range declared.FindAllStringSubmatch(head, -1) {
			assert.Contains(t, body, "$"+m[1], "the %s query declares $%s and never uses it", name, m[1])
		}
	}
}

// A PRIVATE ADDRESS IS A HIT WITHOUT A LIST. A private-network host address
// (10/8, 172.16/12, 192.168/16) has no business in public text, and listing
// one by digest would publish it: a digest of a private address is guessed
// in seconds. So any private IPv4 literal is a hit of its own class, and the
// hit never names it. The range names themselves ("10.0.0.0/8") identify no
// host and are not hits; neither is a public or documentation address, nor
// four numbers inside a longer dotted version. Refs: MGIT-242
func TestJudge_APrivateAddressIsAHitWithoutNamingIt(t *testing.T) {
	tests := []struct {
		text string
		hit  bool
	}{
		{"the box at 10.20.30.40 answered", true},
		{"(172.20.255.254),", true},
		{"ssh 192.168.7.9:22", true},
		{"10.0.0.0/8, 172.16.0.0/12 and 192.168.0.0/16 are denied", false},
		{"172.15.0.1 and 172.32.0.1 lie outside 172.16/12", false},
		{"8.8.8.8 and 192.0.2.10 are not private", false},
		{"version 10.1.2.3.4 or v10.1.2.3", false},
		{"999.1.1.1 is no address", false},
		{"guest ip=10.0.2.15 gateway=10.0.2.2", false},
		{"inet 172.31.16.162/30 via 172.31.16.161", false},
		{"10.0.3.1 is outside the guest network", true},
		{"172.30.0.1 is outside the sandbox net", true},
	}
	for _, tt := range tests {
		t.Run(tt.text, func(t *testing.T) {
			hits := judge([]version{{Field: "description", Rev: "as written", At: cutoff, Text: tt.text}}, testLists(), cutoff)
			assert.Equal(t, tt.hit, len(hits) == 1 && hits[0].Class == "address", "%v", hits)
		})
	}
	var out bytes.Buffer
	report(&out, 7, &collected{counts: map[string]int{}}, judge([]version{{Field: "title", Rev: "as opened", At: cutoff, Text: "10.20.30.40"}}, testLists(), cutoff), cutoff)
	assert.Contains(t, out.String(), "private address in title as opened")
	for _, part := range []string{"10.20", "20.30", "30.40"} {
		assert.NotContains(t, out.String(), part, "the report never names the address")
	}
}
