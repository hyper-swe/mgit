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
	m := antiStubStep.FindSubmatch(wf)
	if m == nil {
		t.Fatal("release.yml: the anti-stub grep step was not found in its expected shape; update this test with the step")
	}
	phrases, err := stubPatternPhrases(string(m[1]))
	if err != nil {
		t.Fatalf("release.yml: %v", err)
	}
	if len(phrases) < 3 {
		t.Fatalf("release.yml: anti-stub pattern %q gave %d phrases, want at least 3", m[1], len(phrases))
	}
	return phrases
}

// stubPatternPhrases splits the step's grep pattern on its alternation into
// the phrases this test compares as literal text.
func stubPatternPhrases(pattern string) ([]string, error) {
	var phrases []string
	for _, p := range strings.Split(pattern, `\|`) {
		if p == "" {
			return nil, fmt.Errorf("empty phrase in the anti-stub pattern %q", pattern)
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
