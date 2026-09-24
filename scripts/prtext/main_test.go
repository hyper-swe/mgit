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
	assert.True(t, strings.HasPrefix(last, "prtext: FAIL — 3 text versions carry a listed word at or after"), last)
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

func lastLine(s string) string {
	lines := strings.Split(strings.TrimSpace(s), "\n")
	return lines[len(lines)-1]
}
