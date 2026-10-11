package packaging

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// Exercise the actual prerequisite script without installing anything: the
// fetch guard records the Go URL and stops before the download or extraction.
func TestLinuxPrereqs_GoToolchainFromModule(t *testing.T) {
	for _, tc := range []struct {
		name, module, override, want string
		bad                          bool
	}{
		{"module_default", "go 1.26.9\n", "", "go1.26.9.linux-amd64.tar.gz", false},
		{"module_changed", "go 1.27.2\n", "", "go1.27.2.linux-amd64.tar.gz", false},
		{"override", "go 1.26.9\n", "1.27.1", "go1.27.1.linux-amd64.tar.gz", false},
		{"missing", "module fixture\n", "", "", true},
		{"invalid", "go NOT_A_VERSION\n", "", "", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root := t.TempDir()
			write := func(name, body string) {
				t.Helper()
				p := filepath.Join(root, name)
				if err := os.MkdirAll(filepath.Dir(p), 0700); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(p, []byte(body), 0700); err != nil { //nolint:gosec // G306: test-owned executable fixture scripts
					t.Fatal(err)
				}
			}
			write("go.mod", tc.module)
			// Force the installed-Go probe to miss in the fixture, independent of
			// the runner's real toolchain. All download/extraction code stays blocked.
			script := strings.ReplaceAll(readRepoFile(t, "scripts/release/linux-build-prereqs.sh"), "/usr/local/go/bin/go", filepath.Join(root, "missing-go"))
			write("scripts/release/linux-build-prereqs.sh", script)
			write("scripts/ci/guard-fetch.sh", "#!/bin/sh\ncase \"$*\" in *go-toolchain-tarball*) printf '%s\\n' \"$*\"; exit 42;; *apt-update*|*apt-install-libkrun-prereqs*) exit 0;; *) exit 43;; esac\n")
			write("bin/id", "#!/bin/sh\necho 0\n")
			write("bin/uname", "#!/bin/sh\necho x86_64\n")
			cmd := exec.Command("bash", filepath.Join(root, "scripts", "release", "linux-build-prereqs.sh")) //nolint:gosec // G204: fixed command and test-owned script path
			cmd.Env = append(os.Environ(), "PATH="+filepath.Join(root, "bin")+":"+os.Getenv("PATH"), "MGIT_GO_VERSION="+tc.override)
			out, err := cmd.CombinedOutput()
			if err == nil {
				t.Fatal("script did not stop at the fixture guard")
			}
			if tc.bad {
				if !strings.Contains(string(out), "invalid Go toolchain") {
					t.Fatalf("expected fail-closed toolchain error; got %s", out)
				}
				return
			}
			if !strings.Contains(string(out), tc.want) {
				t.Fatalf("Go download must use %s; got %s", tc.want, out)
			}
		})
	}
}

func TestGoToolchain_InventoryRejectsDriftAndExtraExemption(t *testing.T) {
	held := strings.Contains(readRepoFile(t, "scripts/ci/check-go-toolchain.py"), `EXEMPT_PATHS = {".github/workflows/release.yml"}`)
	for _, tc := range []struct {
		name, path, text string
		bad, extra       bool
	}{
		{"conflicting_directives", "go.mod", "go 1.26.0\ntoolchain go1.26.9\n", true, false},
		{"equal_directives", "go.mod", "go 1.26.9\ntoolchain go1.26.9\n", false, false},
		{"match", ".github/workflows/ci.yml", "go-version: \"1.26.9\"\n", false, false},
		{"visible_release_hold", ".github/workflows/release.yml", "go-version: \"1.26.6\"\n", !held, false},
		{"workflow", ".github/workflows/ci.yml", "go-version: \"1.26.6\"\n", true, false},
		{"docker", "Dockerfile", "FROM golang:1.26.6-alpine\n", true, false},
		{"docker_lowercase", "docker/Dockerfile.build", "from golang:1.26.6-alpine\n", true, false},
		{"make_variable", "Makefile", "GO_VERSION := 1.26.6\n", true, false},
		{"yaml_variable", ".goreleaser.yaml", "GO_VERSION: \"1.26.6\"\n", true, false},
		{"script", "scripts/build.sh", "GO_VERSION=1.26.6\n", true, false},
		{"fallback", "scripts/build.sh", "GO_VERSION=\"${MGIT_GO_VERSION:-1.26.6}\"\n", true, false},
		{"dl", "Makefile", "go install golang.org/dl/go1.26.6@latest\n", true, false},
		{"toolchain", ".goreleaser.yaml", "GOTOOLCHAIN=go1.26.6\n", true, false},
		{"other_tool", "scripts/build.sh", "GOTOOLCHAIN=local; echo goreleaser v2.17.1\n", false, false},
		{"second_exemption", "scripts/build.sh", "GO_VERSION=1.26.6\n", true, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root := t.TempDir()
			write := func(name, body string) {
				t.Helper()
				p := filepath.Join(root, name)
				if err := os.MkdirAll(filepath.Dir(p), 0700); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(p, []byte(body), 0600); err != nil {
					t.Fatal(err)
				}
			}
			write("go.mod", "go 1.26.9\n")
			script := readRepoFile(t, "scripts/ci/check-go-toolchain.py")
			if tc.extra {
				lines := strings.Split(script, "\n")
				for i, line := range lines {
					if strings.HasPrefix(line, "EXEMPT_PATHS = ") {
						lines[i] = `EXEMPT_PATHS = {".github/workflows/release.yml", "scripts/build.sh"}`
					}
				}
				script = strings.Join(lines, "\n")
			}
			write("scripts/ci/check-go-toolchain.py", script)
			write(tc.path, tc.text)
			cmd := exec.Command("git", "init", "-q", root) //nolint:gosec // G204: fixed arguments and test-owned temporary repository
			if out, err := cmd.CombinedOutput(); err != nil {
				t.Fatalf("git init: %s: %v", out, err)
			}
			cmd = exec.Command("python3", filepath.Join(root, "scripts", "ci", "check-go-toolchain.py"), root) //nolint:gosec // G204: fixed command and test-owned checker/fixture paths
			out, err := cmd.CombinedOutput()
			if (err != nil) != tc.bad {
				t.Fatalf("bad=%v: %s: %v", tc.bad, out, err)
			}
			if tc.name == "visible_release_hold" && held && !strings.Contains(string(out), "EXEMPT .github/workflows/release.yml:1 pin 1.26.6") {
				t.Fatalf("exemption not printed: %s", out)
			}
		})
	}
}
