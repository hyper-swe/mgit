package packaging

import (
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// linux_arm64 ships in every release, and no hosted CI runner exposes KVM on
// arm64, so its daemon is built and verified but never boots a guest before
// a release. Every surface a user reads says so in the same words, so the
// claim cannot drift into "verified" by paraphrase: the install doc, the
// newest release's CHANGELOG section, and the release notes. [Unreleased] is
// held to the same words only where it mentions linux_arm64: an entry about
// something else must not have to restate the claim, or the first entry
// after every cut would turn this red. Refs: MGIT-230.5, MGIT-276
func TestLinuxArm64_EverySurfaceSaysBuildVerifiedNotBootVerified(t *testing.T) {
	changelog := readRepoFile(t, "CHANGELOG.md")
	assert.Empty(t, arm64ChangelogProblems(changelog), "CHANGELOG.md")
	for name, text := range map[string]string{
		"docs/INSTALL-SANDBOX.md":         readRepoFile(t, filepath.Join("docs", "INSTALL-SANDBOX.md")),
		".goreleaser.yaml release header": releaseHeader(t),
	} {
		assert.Regexp(t, arm64Claim, text, "%s must state linux_arm64 as %s, in those words", name, arm64Words)
	}
}

const arm64Words = "build-verified and not boot-verified"

// arm64Claim is the sentence every surface must carry.
var arm64Claim = regexp.MustCompile("`?linux_arm64`? is " + regexp.QuoteMeta(arm64Words))

// arm64ChangelogProblems names what is wrong with a CHANGELOG's linux_arm64
// claim: the newest released section must state it in the words, and
// [Unreleased] must too wherever it mentions linux_arm64 at all.
func arm64ChangelogProblems(changelog string) []string {
	unreleased, released, ok := changelogSections(changelog)
	if !ok {
		return []string{"the CHANGELOG needs an [Unreleased] section followed by a released one"}
	}
	var problems []string
	if !arm64Claim.MatchString(released) {
		problems = append(problems, "the newest release must state linux_arm64 as "+arm64Words)
	}
	if strings.Contains(unreleased, "linux_arm64") && !arm64Claim.MatchString(unreleased) {
		problems = append(problems, "[Unreleased] mentions linux_arm64 without stating it as "+arm64Words)
	}
	return problems
}

// changelogSections splits out [Unreleased] and the newest released section.
func changelogSections(changelog string) (unreleased, released string, ok bool) {
	start := strings.Index(changelog, "\n## [Unreleased]")
	if start < 0 {
		return "", "", false
	}
	rest := changelog[start+1:]
	end := strings.Index(rest[1:], "\n## [")
	if end < 0 {
		return "", "", false
	}
	unreleased, next := rest[:end+1], rest[end+2:]
	if after := strings.Index(next[1:], "\n## ["); after >= 0 {
		next = next[:after+1]
	}
	return unreleased, next, true
}

// The check is held to fixtures, so its silence on the real CHANGELOG means
// something: it must accept an unrelated [Unreleased] entry over a release
// that states the claim, and refuse every way the claim can be missing.
func TestArm64ChangelogProblems_Fixtures(t *testing.T) {
	claim := "- `linux_arm64` is " + arm64Words + ".\n"
	tests := []struct {
		name, changelog string
		want            int
	}{
		{"empty_unreleased_release_states_it", "x\n## [Unreleased]\n\n## [1.0] - d\n" + claim + "\n## [0.9]\nold\n", 0},
		{"unrelated_unreleased_entry", "x\n## [Unreleased]\n- fixed a thing\n\n## [1.0] - d\n" + claim, 0},
		{"release_missing_the_claim", "x\n## [Unreleased]\n\n## [1.0] - d\n- nothing\n", 1},
		{"unreleased_paraphrases_it", "x\n## [Unreleased]\n- linux_arm64 is verified\n\n## [1.0] - d\n" + claim, 1},
		{"unreleased_states_it", "x\n## [Unreleased]\n" + claim + "\n## [1.0] - d\n" + claim, 0},
		{"no_released_section", "x\n## [Unreleased]\n- a\n", 1},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Len(t, arm64ChangelogProblems(tt.changelog), tt.want)
		})
	}
}

// A go-installed mgit-sandboxd on Linux is the CGO-free firecracker build: it
// boots only a kernel + ext4 rootfs image, needs firecracker on PATH, and
// refuses sync and export. The release notes said "Linux works out of the
// box" above that command. Wherever a user-facing surface offers it, the
// Linux caveat must be beside it. Refs: MGIT-230.10
func TestGoInstallOfTheDaemon_NamesTheFirecrackerBuildOnLinux(t *testing.T) {
	for name, text := range map[string]string{
		".goreleaser.yaml release header": releaseHeader(t),
		"README.md":                       readRepoFile(t, "README.md"),
	} {
		assert.NotContains(t, text, "works out of the box", "%s: a go-installed Linux daemon does not", name)
		lines := strings.Split(text, "\n")
		offered := false
		for i, l := range lines {
			if !strings.Contains(l, "go install github.com/hyper-swe/mgit/cmd/mgit-sandboxd") {
				continue
			}
			offered = true
			assert.Contains(t, paragraph(lines, i), "firecracker",
				"%s offers go install of the daemon at line %d without saying that on Linux it is the firecracker build", name, i+1)
		}
		assert.True(t, offered, "%s no longer offers go install of the daemon; update this test's case list", name)
	}
}

// paragraph is the run of non-blank lines around lines[i]: the sentence or
// the commented command block a reader takes in with it.
func paragraph(lines []string, i int) string {
	lo, hi := i, i
	for lo > 0 && strings.TrimSpace(lines[lo-1]) != "" {
		lo--
	}
	for hi < len(lines)-1 && strings.TrimSpace(lines[hi+1]) != "" {
		hi++
	}
	return strings.Join(lines[lo:hi+1], "\n")
}

// releaseHeader is the release notes' header block from .goreleaser.yaml.
func releaseHeader(t *testing.T) string {
	t.Helper()
	cfg := readRepoFile(t, ".goreleaser.yaml")
	at := strings.Index(cfg, "\n  header: |\n")
	require.GreaterOrEqual(t, at, 0, "the release notes header")
	return cfg[at:]
}
