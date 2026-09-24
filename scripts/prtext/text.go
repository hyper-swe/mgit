package main

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"regexp"
	"strings"
	"time"
)

// cutoff: the check gates only text written at or after this instant.
// Hits in earlier text are reported with their place and never gate, so
// the check asks for no cleanup of old pull requests. The instant is the
// one the ruling's application stamped (R-H319, applied 2026-09-24).
var cutoff = time.Date(2026, 9, 24, 10, 4, 0, 0, time.UTC)

// A version is one revision of one piece of pull-request text: the title
// as opened or as renamed, a description or a comment as written or as
// one revision in its edit history. At is when that revision was written;
// the zero time means it could not be read.
type version struct {
	Field string
	Rev   string
	At    time.Time
	Text  string
}

// place names a version for a reader, never quoting it.
func (v version) place() string {
	at := "at an unreadable time"
	if !v.At.IsZero() {
		at = v.At.UTC().Format(time.RFC3339)
	}
	return fmt.Sprintf("%s %s %s", v.Field, v.Rev, at)
}

// lists holds the two digest sets. Terms are matched in the sibling
// repository's hashed-lint form; names only as whole tokens.
type lists struct {
	terms map[string]bool
	names map[string]bool
}

// A hit is a version that carries a listed word. It holds the matched
// digest for the triage file only; the report never prints it.
type hit struct {
	version
	Class  string // "term" or "name"
	Gating bool
	digest string
}

var (
	runRE    = regexp.MustCompile(`[a-z0-9]+`)
	tokenRE  = regexp.MustCompile(`[a-z0-9]+(?:-[a-z0-9]+)*`)
	nonAlnum = regexp.MustCompile(`[^a-z0-9]`)
)

// minLen: a candidate shorter than this is never compared, as in the
// sibling repository's lint; the list generator refuses shorter entries so
// that no entry can sit in a list unable to match.
const minLen = 4

func digest(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:])
}

// normalize is the form a listed word is hashed in: lower case, letters
// and digits only.
func normalize(s string) string {
	return nonAlnum.ReplaceAllString(strings.ToLower(s), "")
}

func addCandidate(set map[string]bool, s string) {
	if len(s) >= minLen {
		set[s] = true
	}
}

// termCandidates is the sibling repository's hashed-lint form: every run of
// letters and digits, every whitespace word with everything else stripped,
// and every pair of adjacent runs joined, so a two-word term is one digest.
func termCandidates(text string) map[string]bool {
	lower := strings.ToLower(text)
	out := map[string]bool{}
	runs := runRE.FindAllString(lower, -1)
	for i, r := range runs {
		addCandidate(out, r)
		if i > 0 {
			addCandidate(out, runs[i-1]+r)
		}
	}
	for _, w := range strings.Fields(lower) {
		addCandidate(out, nonAlnum.ReplaceAllString(w, ""))
	}
	return out
}

// nameCandidates matches whole tokens only: each hyphen-joined token with
// its hyphens removed, and each whitespace word stripped. A name is found
// in "(name)," or "name's", never inside a longer token or split by spaces.
func nameCandidates(text string) map[string]bool {
	lower := strings.ToLower(text)
	out := map[string]bool{}
	for _, t := range tokenRE.FindAllString(lower, -1) {
		addCandidate(out, strings.ReplaceAll(t, "-", ""))
	}
	for _, w := range strings.Fields(lower) {
		addCandidate(out, nonAlnum.ReplaceAllString(w, ""))
	}
	return out
}

// anyListed returns the digest of a candidate the set lists, or "".
func anyListed(candidates, set map[string]bool) string {
	for c := range candidates {
		if d := digest(c); set[d] {
			return d
		}
	}
	return ""
}

// judge finds the versions that carry a listed word, one hit per version
// and class. A version written at or after the cutoff gates, and so does
// one whose time could not be read; an earlier one is reported only.
func judge(vs []version, l lists, cutoff time.Time) []hit {
	var hits []hit
	for _, v := range vs {
		gating := v.At.IsZero() || !v.At.Before(cutoff)
		if d := anyListed(termCandidates(v.Text), l.terms); d != "" {
			hits = append(hits, hit{version: v, Class: "term", Gating: gating, digest: d})
		}
		if d := anyListed(nameCandidates(v.Text), l.names); d != "" {
			hits = append(hits, hit{version: v, Class: "name", Gating: gating, digest: d})
		}
	}
	return hits
}

// report writes what was read and every hit by its place, never the word,
// and returns the exit code: 1 when a hit gates, else 0. Its last line is
// the one a commit status carries.
func report(w io.Writer, pr int, got *collected, hits []hit, cutoff time.Time) int {
	c := got.counts
	fmt.Fprintf(w, "prtext: #%d — read %d text versions (title %d, description %d, comments %d, reviews %d, review comments %d, edit revisions %d)\n",
		pr, len(got.versions), c["title"], c["description"], c["comments"], c["reviews"], c["review comments"], c["revisions"])
	if got.deleted > 0 {
		fmt.Fprintf(w, "prtext: %d revision deleted from the history — nothing left to read\n", got.deleted)
	}
	var gating []string
	for _, h := range hits {
		verdict := "reported, not gating (before the cutoff)"
		if h.Gating {
			verdict = "gating (at or after the cutoff)"
			gating = append(gating, h.place())
		}
		fmt.Fprintf(w, "prtext: listed %s in %s — %s\n", h.Class, h.place(), verdict)
	}
	stamp := cutoff.UTC().Format(time.RFC3339)
	if len(gating) > 0 {
		fmt.Fprintf(w, "prtext: FAIL — %d text versions carry a listed word at or after %s: %s\n", len(gating), stamp, strings.Join(gating, "; "))
		return 1
	}
	fmt.Fprintf(w, "prtext: PASS — no listed word at or after %s; %d earlier hits reported, not gating\n", stamp, len(hits))
	return 0
}

// writeHits records each hit with its matched digest, tab-separated, for
// the maintainers' triage against their private word list. It is written
// only to the file the sweep is given, never to the output.
func writeHits(w io.Writer, pr int, hits []hit) {
	for _, h := range hits {
		g := "reported"
		if h.Gating {
			g = "gating"
		}
		at := ""
		if !h.At.IsZero() {
			at = h.At.UTC().Format(time.RFC3339)
		}
		fmt.Fprintf(w, "%d\t%s\t%s\t%s\t%s\t%s\t%s\n", pr, h.Field, h.Rev, at, h.Class, g, h.digest)
	}
}
