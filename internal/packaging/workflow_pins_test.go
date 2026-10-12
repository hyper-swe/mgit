package packaging

import (
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"
)

// usesLine finds a step's `uses:` reference and any trailing comment.
var usesLine = regexp.MustCompile(`^\s*(?:-\s+)?uses:\s*["']?([^\s"'#]+)["']?\s*(#.*)?$`)

// pinned is a third-party reference fixed to a full commit SHA.
var pinned = regexp.MustCompile(`^[\w.-]+/[\w./-]+@[0-9a-f]{40}$`)

// versionComment names the release the SHA was taken from.
var versionComment = regexp.MustCompile(`^#\s*v\d+(\.\d+)*\b`)

// EVERY THIRD-PARTY ACTION IS PINNED TO A FULL COMMIT SHA (MGIT-246). A tag
// such as @v4 can be moved to other code by whoever controls the action's
// repository, and the workflow then runs that code with its own
// permissions: the release workflow's include the signing identity. A
// 40-hex commit SHA cannot move. Each pin carries the release it was taken
// from in a trailing comment, so a reader and an updater can see the
// version. Local references (./...) are this repository's own code and are
// exempt. The update procedure is in CONTRIBUTING.md, "Workflow actions are
// pinned". Refs: MGIT-246
func TestWorkflows_PinEveryThirdPartyActionToAFullCommitSHA(t *testing.T) {
	files, err := filepath.Glob(filepath.Join(repoRoot(t), ".github", "workflows", "*.y*ml"))
	if err != nil || len(files) == 0 {
		t.Fatalf("no workflow files found: %v", err)
	}
	seen := 0
	for _, f := range files {
		raw, err := os.ReadFile(f) //nolint:gosec // G304: test-only; the workflow files of this repository
		if err != nil {
			t.Fatal(err)
		}
		for i, line := range strings.Split(string(raw), "\n") {
			m := usesLine.FindStringSubmatch(line)
			if len(m) < 3 || strings.HasPrefix(m[1], "./") {
				continue
			}
			seen++
			where := filepath.Base(f) + ":" + strconv.Itoa(i+1)
			if !pinned.MatchString(m[1]) {
				t.Errorf("%s: %s is not pinned to a full commit SHA", where, m[1])
				continue
			}
			if !versionComment.MatchString(strings.TrimSpace(m[2])) {
				t.Errorf("%s: %s carries no '# vX.Y.Z' comment naming the release it was taken from", where, m[1])
			}
		}
	}
	if seen == 0 {
		t.Fatal("no third-party uses: found — the pattern no longer reads the workflows")
	}
}

// The pattern reads every form a step can take, and judges each correctly:
// a negative control for the test above.
func TestWorkflowPins_ThePatternReadsEveryForm(t *testing.T) {
	sha := strings.Repeat("a1", 20)
	tests := []struct {
		line   string
		ok     bool
		exempt bool
	}{
		{"      - uses: actions/checkout@" + sha + " # v4.4.0", true, false},
		{"        uses: 'owner/action/sub@" + sha + "' # v1", true, false},
		{"      - uses: actions/checkout@v4", false, false},
		{"      - uses: actions/checkout@" + sha, false, false},
		{"      - uses: actions/checkout@" + sha[:39] + " # v4", false, false},
		{"    uses: ./.github/workflows/e2e.yml", true, true},
	}
	for _, tt := range tests {
		m := usesLine.FindStringSubmatch(tt.line)
		if m == nil {
			t.Errorf("not read: %q", tt.line)
			continue
		}
		if exempt := strings.HasPrefix(m[1], "./"); exempt != tt.exempt {
			t.Errorf("%q: exempt %v, want %v", tt.line, exempt, tt.exempt)
			continue
		}
		ok := tt.exempt || pinned.MatchString(m[1]) && versionComment.MatchString(strings.TrimSpace(m[2]))
		if ok != tt.ok {
			t.Errorf("%q: judged %v, want %v", tt.line, ok, tt.ok)
		}
	}
}
