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
// CHANGELOG's unreleased section, and the release notes. Refs: MGIT-230.5
func TestLinuxArm64_EverySurfaceSaysBuildVerifiedNotBootVerified(t *testing.T) {
	const words = "build-verified and not boot-verified"
	changelog := readRepoFile(t, "CHANGELOG.md")
	end := strings.Index(changelog, "\n## [0.")
	require.Positive(t, end, "the CHANGELOG has a released section after [Unreleased]")
	unreleased := changelog[:end]
	for name, text := range map[string]string{
		"docs/INSTALL-SANDBOX.md":         readRepoFile(t, filepath.Join("docs", "INSTALL-SANDBOX.md")),
		"CHANGELOG.md [Unreleased]":       unreleased,
		".goreleaser.yaml release header": releaseHeader(t),
	} {
		assert.Regexp(t, "`?linux_arm64`? is "+regexp.QuoteMeta(words), text,
			"%s must state linux_arm64 as %s, in those words", name, words)
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
