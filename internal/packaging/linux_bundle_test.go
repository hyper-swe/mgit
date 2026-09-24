package packaging

import (
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// The Linux release ships the libkrun-backed daemon with libkrun and
// libkrunfw bundled beside it: no Ubuntu release packages either library, and
// firecracker — the daemon the Linux archives used to carry — refuses the sync
// and export the agent loop lives on (hyper-swe/mgit#12). The daemon is built
// by scripts/release/build-linux-sandboxd.sh in ubuntu:20.04 and handed to
// goreleaser through scripts/release/gobinary-prebuilt.sh, because the
// release runs on a macOS runner and open-source goreleaser cannot build a
// cgo Linux binary there. These tests pin that shape in the files that make
// it. Refs: MGIT-229, ADR-016

// yamlBlock returns the text of the YAML list item or section that starts at
// the line containing header, up to the next line indented no deeper than
// header's own line — so an assertion applies to ONE build, not the file.
func yamlBlock(t *testing.T, cfg, header string) string {
	t.Helper()
	lines := strings.Split(cfg, "\n")
	for i, line := range lines {
		if !strings.Contains(line, header) {
			continue
		}
		indent := len(line) - len(strings.TrimLeft(line, " "))
		end := len(lines)
		for j := i + 1; j < len(lines); j++ {
			l := lines[j]
			if strings.TrimSpace(l) == "" || strings.HasPrefix(strings.TrimSpace(l), "#") {
				continue
			}
			if len(l)-len(strings.TrimLeft(l, " ")) <= indent {
				end = j
				break
			}
		}
		return strings.Join(lines[i:end], "\n")
	}
	t.Fatalf("no line containing %q", header)
	return ""
}

// topLevelBlock returns a top-level YAML section (the line reading exactly
// "<key>:" and everything indented beneath it).
func topLevelBlock(t *testing.T, cfg, key string) string {
	t.Helper()
	_, rest, ok := strings.Cut("\n"+cfg, "\n"+key+":\n")
	require.True(t, ok, "no top-level %q section", key)
	var b strings.Builder
	for _, l := range strings.Split(rest, "\n") {
		if l != "" && !strings.HasPrefix(l, " ") && !strings.HasPrefix(l, "#") {
			break
		}
		b.WriteString(l + "\n")
	}
	return b.String()
}

func TestGoreleaser_LinuxSandboxdIsTheBundledLibkrunDaemon(t *testing.T) {
	block := yamlBlock(t, readRepoFile(t, ".goreleaser.yaml"), "- id: mgit-sandboxd-linux")
	assert.Contains(t, block, "tool: ./scripts/release/gobinary-prebuilt.sh",
		"the Linux daemon must come from the ubuntu:20.04 assembler, never a CGO-free compile of the firecracker build")
	assert.NotContains(t, block, "CGO_ENABLED=0",
		"the Linux daemon links libkrun through cgo; a CGO_ENABLED=0 here would state the opposite of what ships")
	// goreleaser EXECS its build tool; the self-test runs it through bash and
	// cannot see a missing executable bit, which is how the first CI run of
	// this config failed with "fork/exec …: permission denied".
	info, err := os.Stat(filepath.Join(repoRoot(t), "scripts", "release", "gobinary-prebuilt.sh"))
	require.NoError(t, err)
	assert.NotZero(t, info.Mode().Perm()&0o111, "scripts/release/gobinary-prebuilt.sh must be executable")
	assert.Regexp(t, `goos:\s*\n\s*- linux\s*\n\s*goarch:`, block, "linux only")
	for _, arch := range []string{"- amd64", "- arm64"} {
		assert.Contains(t, block, arch)
	}
}

// Every binary is stamped with the COMMIT date, not the build time: the Linux
// daemon is built in a separate job, and release_smoke.sh requires mgit and
// mgit-sandboxd of one archive to report one build, byte for byte.
func TestGoreleaser_EveryBuildStampsTheCommitDate(t *testing.T) {
	cfg := readRepoFile(t, ".goreleaser.yaml")
	stamps := regexp.MustCompile(`buildinfo\.date=([^\s"]+)`).FindAllStringSubmatch(cfg, -1)
	require.NotEmpty(t, stamps)
	for _, m := range stamps {
		assert.Equal(t, "{{.CommitDate}}", strings.ReplaceAll(m[1], " ", ""), "buildinfo.date stamp")
	}
}

func TestGoreleaser_LinuxArchivesCarryTheBundle(t *testing.T) {
	archives := topLevelBlock(t, readRepoFile(t, ".goreleaser.yaml"), "archives")
	for _, want := range []string{
		`src: "dist/mgit-sandboxd-linux_{{ .Os }}_{{ .Arch }}*/lib/*"`,
		`dst: "lib/"`,
		`src: "dist/mgit-sandboxd-linux_{{ .Os }}_{{ .Arch }}*/THIRD_PARTY/*"`,
		`dst: "THIRD_PARTY/"`,
	} {
		assert.Contains(t, archives, want)
	}
}

// libkrunfw compiles a GPL-2.0 kernel into itself, so every release that
// bundles it publishes that kernel's corresponding source — and the release's
// signed checksums must cover it like any other asset.
func TestGoreleaser_ReleaseAndChecksumsCarryTheCorrespondingSource(t *testing.T) {
	cfg := readRepoFile(t, ".goreleaser.yaml")
	for _, section := range []string{"checksum", "release"} {
		block := topLevelBlock(t, cfg, section)
		assert.Contains(t, block, "- glob: ./dist-sources/*", "%s: must carry the corresponding-source assets", section)
	}
}

// The daemon is built in ONE job, e2e.yml's linux-sandboxd: every pull
// request exercises it, and the release (which runs e2e.yml as its gate)
// ships the artifacts that job uploaded in the same run — the bytes the
// user-path leg booted a guest with.
func TestE2EWorkflow_BuildsTheLinuxDaemonInUbuntu2004ForBothArches(t *testing.T) {
	job := jobBlock(t, readRepoFile(t, ".github/workflows/e2e.yml"), "linux-sandboxd")
	for _, want := range []string{
		"image: ubuntu:20.04",
		"runner: ubuntu-latest",
		"runner: ubuntu-24.04-arm",
		"scripts/release/linux-build-prereqs.sh",
		"scripts/release/build-linux-sandboxd.sh",
		"fetch-depth: 0",
		`v="${GITHUB_REF_NAME#v}"`,
		"name: linux-sandboxd-${{ matrix.arch }}",
		"name: linux-sources",
	} {
		assert.Contains(t, job, want, "the linux-sandboxd job must carry %q", want)
	}
	assert.NotContains(t, job, "MGIT_LIBKRUN_CACHE: ~", "a YAML ~ is never expanded (MGIT-238)")
}

func TestE2EWorkflow_UserPathBootsTheBundledDaemonWithNonSnapshotStamps(t *testing.T) {
	job := jobBlock(t, readRepoFile(t, ".github/workflows/e2e.yml"), "linux-user-path")
	for _, want := range []string{
		"needs: linux-sandboxd",
		"name: linux-sandboxd-amd64",
		"MGIT_LINUX_PREBUILT:",
		`git tag "v0.0.0-ci.$GITHUB_RUN_ID"`,
		"scripts/release/verify-linux-sandboxd.sh",
		"scripts/e2e/linux_user_path.sh",
	} {
		assert.Contains(t, job, want, "the linux-user-path job must carry %q", want)
	}
	assert.NotContains(t, job, "--snapshot", "the stamp check a release relies on must run here")
	assert.NotContains(t, job, "fetch-firecracker", "the Linux user path no longer needs firecracker")
}

func TestReleaseWorkflow_ShipsTheGatesLinuxDaemonsAndTheKernelSource(t *testing.T) {
	release := jobBlock(t, readRepoFile(t, ".github/workflows/release.yml"), "release")
	for _, want := range []string{
		"needs: [preflight, e2e]",
		"pattern: linux-sandboxd-*",
		"path: dist-prebuilt",
		"name: linux-sources",
		"path: dist-sources",
		"MGIT_LINUX_PREBUILT: ${{ github.workspace }}/dist-prebuilt",
	} {
		assert.Contains(t, release, want, "the release job must carry %q", want)
	}
}

// The shim's refusals are the release's last line of defense against shipping
// the wrong daemon; its self-test runs here so every `go test` exercises them.
func TestGobinaryPrebuilt_SelfTest(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("the release scripts are bash")
	}
	//nolint:gosec // G204: a fixed repo-relative script path; no input reaches the argv
	cmd := exec.Command("bash", filepath.Join("scripts", "release", "gobinary-prebuilt-selftest.sh"))
	cmd.Dir = repoRoot(t)
	out, err := cmd.CombinedOutput()
	require.NoError(t, err, "%s", out)
	assert.Contains(t, string(out), "gobinary-prebuilt selftest: PASS")
}
