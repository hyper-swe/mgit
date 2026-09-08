// The reviewer-verdict-at-head gate's decision table, each row a comment
// fixture laid out by hand (the mechanism the swe repository built first;
// MGIT-201 copies it). Refs: MGIT-201
package main

import (
	"bytes"
	"strings"
	"testing"
)

const head = "8a29f48c0ffee0ddba11fee1dea5c0ffee0ddba1"

func run(t *testing.T, comments ...comment) (int, string) {
	t.Helper()
	var out bytes.Buffer
	code := gate(head, comments, &out)
	return code, out.String()
}

func TestAPassAtHeadIsGreenAndSaysWhatItFound(t *testing.T) {
	code, out := run(t, comment{Author: "reviewer", Created: "2026-09-08T04:54:22Z",
		Body: "VERDICT: PASS at 8a29f48 — gate review, run not read.\n1 PASS …"})
	if code != 0 {
		t.Fatalf("a PASS naming the head is green, got %d:\n%s", code, out)
	}
	for _, want := range []string{"looked for " + head, "found PASS at 8a29f48", "reviewer", "2026-09-08T04:54:22Z"} {
		if !strings.Contains(out, want) {
			t.Errorf("the report names what it looked for and what it found (%q):\n%s", want, out)
		}
	}
}

func TestAStalePassIsRed(t *testing.T) {
	code, out := run(t, comment{Author: "reviewer", Created: "2026-09-08T04:00:00Z",
		Body: "VERDICT: PASS at 929127a — gate review, run not read."})
	if code == 0 {
		t.Fatalf("a PASS naming another sha must not pass a moved head:\n%s", out)
	}
	for _, want := range []string{"looked for " + head, "found PASS at 929127a", "stale", "re-review"} {
		if !strings.Contains(out, want) {
			t.Errorf("the report says the PASS is stale and names both shas (%q):\n%s", want, out)
		}
	}
}

func TestAFailAtHeadIsRed(t *testing.T) {
	code, out := run(t, comment{Author: "reviewer", Created: "2026-09-08T04:00:00Z",
		Body: "VERDICT: FAIL at 8a29f48 (line 3) — closable by one commit."})
	if code == 0 {
		t.Fatalf("a FAIL at head is red:\n%s", out)
	}
	if !strings.Contains(out, "found FAIL at 8a29f48") {
		t.Errorf("the report names the FAIL:\n%s", out)
	}
}

func TestNoVerdictIsNotCheckedNeverGreen(t *testing.T) {
	code, out := run(t,
		comment{Author: "author", Created: "2026-09-08T03:00:00Z", Body: "CI's own run of the changed check at this head …"},
		comment{Author: "author", Created: "2026-09-08T03:10:00Z", Body: "Follow-up at the same head 7313f10: the line-2 NOT CHECKED …"})
	if code == 0 {
		t.Fatalf("no verdict comment must read NOT CHECKED and fail:\n%s", out)
	}
	for _, want := range []string{"NOT CHECKED", "no verdict comment", "looked for " + head} {
		if !strings.Contains(out, want) {
			t.Errorf("(%q):\n%s", want, out)
		}
	}
	if code, out := run(t); code == 0 || !strings.Contains(out, "NOT CHECKED") {
		t.Errorf("no comments at all is NOT CHECKED too: %d\n%s", code, out)
	}
}

func TestTheNewestVerdictWins(t *testing.T) {
	// FAIL, then a re-stamp PASS at the same head: the newest wins
	code, out := run(t,
		comment{Author: "reviewer", Created: "2026-09-08T03:52:56Z", Body: "VERDICT: FAIL at 8a29f48 (line 3) — gate review."},
		comment{Author: "reviewer", Created: "2026-09-08T03:58:04Z", Body: "VERDICT: PASS at 8a29f48 (re-stamp) — the line-3 FAIL is closed."})
	if code != 0 {
		t.Fatalf("the newest verdict wins: %d\n%s", code, out)
	}
	// PASS at head, then a later FAIL at head: red
	code, out = run(t,
		comment{Author: "reviewer", Created: "2026-09-08T03:52:56Z", Body: "VERDICT: PASS at 8a29f48 — gate review."},
		comment{Author: "reviewer", Created: "2026-09-08T03:58:04Z", Body: "VERDICT: FAIL at 8a29f48 (line 5) — a later find."})
	if code == 0 {
		t.Fatalf("a later FAIL overrides an earlier PASS: %d\n%s", code, out)
	}
	// order in the slice does not matter; the timestamp does
	code, _ = run(t,
		comment{Author: "reviewer", Created: "2026-09-08T03:58:04Z", Body: "VERDICT: PASS at 8a29f48 (re-stamp)."},
		comment{Author: "reviewer", Created: "2026-09-08T03:52:56Z", Body: "VERDICT: FAIL at 8a29f48 (line 3)."})
	if code != 0 {
		t.Fatal("the newest by time wins, whatever the order given")
	}
}

func TestAVerdictIsReadFromTheFirstLineOnly(t *testing.T) {
	// a quoted verdict inside a discussion is not a verdict
	code, out := run(t, comment{Author: "author", Created: "2026-09-08T05:00:00Z",
		Body: "Replying to the review:\nVERDICT: PASS at 8a29f48 was what I hoped for.\n"})
	if code == 0 || !strings.Contains(out, "NOT CHECKED") {
		t.Fatalf("a verdict line not at the head of a comment is not a verdict: %d\n%s", code, out)
	}
	// a full sha and a short sha both name the head; a wrong prefix does not
	if code, _ := run(t, comment{Author: "r", Created: "2026-09-08T05:00:00Z", Body: "VERDICT: PASS at " + head + " — full sha."}); code != 0 {
		t.Error("the full head sha names the head")
	}
	if code, _ := run(t, comment{Author: "r", Created: "2026-09-08T05:00:00Z", Body: "VERDICT: PASS at 8a29f4 — six hex is too short."}); code == 0 {
		t.Error("fewer than seven hex characters do not name a head")
	}
}
