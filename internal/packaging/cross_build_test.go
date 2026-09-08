package packaging

import (
	"regexp"
	"sort"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// The cross-build gate (scripts/ci/cross-build.sh) compiles every target the
// release ships, on every PR, because the assembler's matrix and CI's own
// platform diverged for a day and the Windows targets of mgit broke unseen
// (MGIT-198). Its target list is read from the release config here — the
// assembler is the source, the script is what must keep up — so a target
// added to .goreleaser.yaml without the gate fails this test. The darwin
// mgit-sandboxd is CGO (libkrun) and is the one target a Linux runner cannot
// compile; the macOS libkrun job builds it. Refs: MGIT-198
func TestCrossBuild_CoversEveryReleaseTarget(t *testing.T) {
	cfg := readRepoFile(t, ".goreleaser.yaml")
	script := readRepoFile(t, "scripts/ci/cross-build.sh")

	want := releaseTargets(t, cfg)
	require.NotEmpty(t, want, "no CGO-free build targets found in the release config; the parser has drifted")
	got := scriptTargets(t, script)

	sort.Strings(want)
	sort.Strings(got)
	assert.Equal(t, want, got, "scripts/ci/cross-build.sh must list exactly the release's CGO-free targets")
}

// releaseTargets returns "<goos>/<goarch> <main>" for every CGO-free build id
// in the goreleaser config, by a line scan of its indentation (no YAML
// dependency, as the sibling tests do).
func releaseTargets(t *testing.T, cfg string) []string {
	t.Helper()
	out := make([]string, 0, 16)
	var mainPkg string
	var goos, goarch []string
	cgo := true
	inGoos, inGoarch := false, false
	flush := func() {
		if mainPkg == "" || cgo {
			return
		}
		for _, o := range goos {
			for _, a := range goarch {
				out = append(out, o+"/"+a+" "+mainPkg)
			}
		}
	}
	for _, raw := range strings.Split(cfg, "\n") {
		line := strings.TrimRight(raw, " ")
		trimmed := strings.TrimSpace(line)
		switch {
		case strings.HasPrefix(trimmed, "- id:") && strings.HasPrefix(line, "  - id:"):
			flush()
			mainPkg, goos, goarch, cgo = "", nil, nil, true
			inGoos, inGoarch = false, false
		case strings.HasPrefix(trimmed, "main:"):
			mainPkg = strings.TrimSuffix(strings.TrimSpace(strings.TrimPrefix(trimmed, "main:")), "/")
		case trimmed == "- CGO_ENABLED=0":
			cgo = false
		case trimmed == "goos:":
			inGoos, inGoarch = true, false
		case trimmed == "goarch:":
			inGoos, inGoarch = false, true
		case strings.HasPrefix(trimmed, "- ") && (inGoos || inGoarch):
			v := strings.TrimSpace(strings.TrimPrefix(trimmed, "- "))
			if inGoos {
				goos = append(goos, v)
			} else {
				goarch = append(goarch, v)
			}
		case strings.HasPrefix(line, "archives:"):
			flush()
			return out
		case trimmed != "" && !strings.HasPrefix(trimmed, "-") && !strings.HasPrefix(trimmed, "#"):
			inGoos, inGoarch = false, false
		}
	}
	flush()
	return out
}

// scriptTargets returns the "<goos>/<goarch> <pkg>" lines of the gate's
// TARGETS block.
func scriptTargets(t *testing.T, script string) []string {
	t.Helper()
	re := regexp.MustCompile(`(?m)^(linux|darwin|windows)/(amd64|arm64)\s+(\S+)$`)
	out := make([]string, 0, 16)
	for _, m := range re.FindAllStringSubmatch(script, -1) {
		out = append(out, m[1]+"/"+m[2]+" "+strings.TrimSuffix(m[3], "/"))
	}
	return out
}
