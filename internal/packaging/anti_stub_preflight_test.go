package packaging

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"
)

// The release workflow's preflight refuses to ship when a non-test Go file
// carries one of its quoted stub phrases. That check runs only when a tag is
// pushed, so a phrase that lands on main is found at release time, after the
// tag exists (v0.8.0 failed this way). This test runs the same scan on every
// change. It reads the phrases from the workflow's own step, so the two
// cannot drift apart.

var antiStubStep = regexp.MustCompile(`(?m)^\s*if grep -rn '([^']+)' \\\s*$\s*--include='\*\.go' --exclude='\*_test\.go' \.; then`)

func antiStubPhrases(t *testing.T, root string) []string {
	t.Helper()
	wf, err := os.ReadFile(filepath.Join(root, ".github", "workflows", "release.yml"))
	if err != nil {
		t.Fatalf("read release workflow: %v", err)
	}
	phrases, err := workflowStubPhrases(wf)
	if err != nil {
		t.Fatalf("release.yml: %v", err)
	}
	return phrases
}

// workflowStubPhrases finds the anti-stub grep step in a workflow's text and
// returns the phrases of its pattern.
func workflowStubPhrases(wf []byte) ([]string, error) {
	m := antiStubStep.FindSubmatch(wf)
	if m == nil {
		return nil, fmt.Errorf("the anti-stub grep step was not found in its expected shape; update this test with the step")
	}
	phrases, err := stubPatternPhrases(string(m[1]))
	if err != nil {
		return nil, err
	}
	if len(phrases) < 3 {
		return nil, fmt.Errorf("anti-stub pattern %q gave %d phrases, want at least 3", m[1], len(phrases))
	}
	return phrases, nil
}

// breSpecial is every character a basic regular expression (grep's default)
// can give a meaning other than itself: any character, bracket expressions,
// repetition, anchors and escapes.
const breSpecial = `.[]*^$\`

// stubPatternPhrases splits the step's grep pattern on its alternation into
// the phrases this test compares as literal text, refusing any phrase whose
// meaning in grep could differ from its literal text.
func stubPatternPhrases(pattern string) ([]string, error) {
	if strings.ContainsAny(pattern, "\n\r") {
		return nil, fmt.Errorf("anti-stub pattern %q holds a newline; grep takes each line as a pattern of its own, so keep the pattern on one line", pattern)
	}
	var phrases []string
	for _, p := range strings.Split(pattern, `\|`) {
		if p == "" {
			return nil, fmt.Errorf("empty phrase in the anti-stub pattern %q", pattern)
		}
		if i := strings.IndexAny(p, breSpecial); i >= 0 {
			return nil, fmt.Errorf("anti-stub phrase %q holds %q, which grep reads as basic regular expression syntax; this check compares literal text, so extend it before using that syntax in the step", p, p[i])
		}
		phrases = append(phrases, p)
	}
	return phrases, nil
}

func TestReleasePreflightAntiStubScanIsClean(t *testing.T) {
	root := repoRoot(t)
	phrases := antiStubPhrases(t, root)
	var hits []string
	err := filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() || !d.Type().IsRegular() {
			return nil
		}
		name := d.Name()
		if !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			return nil
		}
		src, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		for i, line := range bytes.Split(src, []byte("\n")) {
			for _, p := range phrases {
				if bytes.Contains(line, []byte(p)) {
					rel, _ := filepath.Rel(root, path)
					hits = append(hits, rel+":"+strconv.Itoa(i+1)+": "+strings.TrimSpace(string(line)))
				}
			}
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walk: %v", err)
	}
	if len(hits) > 0 {
		t.Errorf("the release preflight would fail on %d line(s) (phrases %q):\n%s", len(hits), phrases, strings.Join(hits, "\n"))
	}
}

// grep reads the step's pattern as a basic regular expression, and this test
// compares phrases as literal text. Those agree only while no phrase holds a
// character grep treats as special, so such a phrase is refused with a reason
// instead of being checked by a rule that differs from the workflow's.
func TestStubPatternPhrasesRefusesRegexSyntax(t *testing.T) {
	got, err := stubPatternPhrases(`"not yet implemented"\|"not implemented"\|"integration pending"`)
	if err != nil || len(got) != 3 || got[1] != `"not implemented"` {
		t.Fatalf("the workflow's current pattern: got %q, %v; want its three phrases", got, err)
	}
	for _, bad := range []string{
		`"not implemented"\|"work pending."`, // grep matches "work pending!" here
		`"stub"\|"to[dD]o"`,
		`"stub"\|"later*"`,
		`^"stub"`,
		`"stub"$`,
		`"stub"\|"a\.b"`,
		`"stub"\|"a]b"`,
	} {
		if p, err := stubPatternPhrases(bad); err == nil {
			t.Errorf("pattern %q: accepted as literal phrases %q, but grep reads it as a regular expression", bad, p)
		} else if !strings.Contains(err.Error(), "regular expression") {
			t.Errorf("pattern %q: error %q does not say why it was refused", bad, err)
		}
	}
	if _, err := stubPatternPhrases(`"a"\|\|"b"`); err == nil {
		t.Error(`an empty alternative was accepted`)
	}
}

// grep takes each line of a pattern argument as a pattern of its own, so a
// phrase put on the next line of the quoted argument is one more phrase there,
// while this test would keep it inside a literal and scan one line at a time.
func TestWorkflowStubPhrasesRefusesAMultilinePattern(t *testing.T) {
	one := "      - name: Anti-stub check\n        run: |\n          if grep -rn '\"a\"\\|\"b\"\\|\"c\"' \\\n            --include='*.go' --exclude='*_test.go' .; then\n"
	if got, err := workflowStubPhrases([]byte(one)); err != nil || len(got) != 3 {
		t.Fatalf("single-line fixture: got %q, %v; want three phrases", got, err)
	}
	two := "      - name: Anti-stub check\n        run: |\n          if grep -rn '\"a\"\\|\"b\"\\|\"c\"\n          \"d\"' \\\n            --include='*.go' --exclude='*_test.go' .; then\n"
	got, err := workflowStubPhrases([]byte(two))
	if err == nil {
		t.Fatalf("two-line fixture: accepted as phrases %q, but grep reads its second line as another pattern", got)
	}
	if !strings.Contains(err.Error(), "newline") {
		t.Errorf("two-line fixture: error %q does not say why it was refused", err)
	}
}
