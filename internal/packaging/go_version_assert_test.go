package packaging

import (
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

func TestGoVersion_AssertActualCompiler(t *testing.T) {
	for _, tc := range []struct {
		name, version string
		bad           bool
	}{
		{"exact", "go1.26.9", false}, {"unpatched", "go1.26.0", true}, {"previous", "go1.26.6", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root := t.TempDir()
			write := func(name, text string, mode os.FileMode) {
				t.Helper()
				p := filepath.Join(root, name)
				if err := os.MkdirAll(filepath.Dir(p), 0700); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(p, []byte(text), mode); err != nil { //nolint:gosec // G306: executable test-owned fixtures
					t.Fatal(err)
				}
			}
			write("go.mod", "go 1.26.9\n", 0600)
			write("scripts/ci/assert-go-version.sh", readRepoFile(t, "scripts/ci/assert-go-version.sh"), 0600)
			write("bin/go", "#!/bin/sh\n[ \"$GOTOOLCHAIN\" = local ] || exit 22\nprintf 'go version "+tc.version+" linux/amd64\\n'\n", 0700)
			cmd := exec.Command("bash", filepath.Join(root, "scripts", "ci", "assert-go-version.sh")) //nolint:gosec // G204: fixed command and test-owned fixture paths
			cmd.Env = append(os.Environ(), "PATH="+filepath.Join(root, "bin")+":"+os.Getenv("PATH"), "GOTOOLCHAIN=auto")
			out, err := cmd.CombinedOutput()
			if (err != nil) != tc.bad {
				t.Fatalf("bad=%v; %s: %v", tc.bad, out, err)
			}
			if !strings.Contains(string(out), "go version "+tc.version) {
				t.Fatalf("actual compiler not printed: %s", out)
			}
		})
	}
}

func TestGoVersion_ModuleDirectivesAgree(t *testing.T) {
	text := readRepoFile(t, "go.mod")
	goLine := regexp.MustCompile(`(?m)^go (\d+\.\d+\.\d+)\s*$`).FindStringSubmatch(text)
	if len(goLine) != 2 {
		t.Fatal("go.mod must specify an exact Go patch version")
	}
	tool := regexp.MustCompile(`(?m)^toolchain go(\S+)`).FindStringSubmatch(text)
	if len(tool) > 0 && tool[1] != goLine[1] {
		t.Fatalf("setup-go reads go %s, not conflicting toolchain %s", goLine[1], tool[1])
	}
}

func goJobGuardProblems(text string) []string {
	start := strings.Index(text, "jobs:\n")
	if start < 0 {
		return nil
	}
	text = text[start+len("jobs:\n"):]
	goCommand := regexp.MustCompile(`(?:run:\s*|^\s*)go (?:test|build|run|install)\b`)
	jobs := regexp.MustCompile(`(?m)^  [\w-]+:\s*$`).FindAllStringIndex(text, -1)
	var problems []string
	for i, pos := range jobs {
		end := len(text)
		if i+1 < len(jobs) {
			end = jobs[i+1][0]
		}
		body := text[pos[0]:end]
		pending := false
		for _, line := range strings.Split(body, "\n") {
			switch {
			case strings.Contains(line, "uses: actions/setup-go@") || strings.Contains(line, "bash scripts/release/linux-build-prereqs.sh"):
				if pending {
					problems = append(problems, "installation without following assertion")
				}
				pending = true
			case strings.Contains(line, "bash scripts/ci/assert-go-version.sh"):
				if !pending {
					problems = append(problems, "assertion before installation")
				}
				pending = false
			case pending && goCommand.MatchString(line):
				problems = append(problems, "Go command before version assertion")
			}
		}
		if pending {
			problems = append(problems, "missing installed-Go assertion")
		}
	}
	return problems
}

func TestGoVersion_EveryWorkflowGoJobAssertsInstalledVersion(t *testing.T) {
	files, err := filepath.Glob(filepath.Join(repoRoot(t), ".github", "workflows", "*.yml"))
	if err != nil {
		t.Fatal(err)
	}
	held := strings.Contains(readRepoFile(t, "scripts/ci/check-go-toolchain.py"), `EXEMPT_PATHS = {".github/workflows/release.yml"}`)
	for _, file := range files {
		if held && filepath.Base(file) == "release.yml" {
			continue
		}
		raw, err := os.ReadFile(file) //nolint:gosec // G304: repository-owned workflow paths
		if err != nil {
			t.Fatal(err)
		}
		if problems := goJobGuardProblems(string(raw)); len(problems) > 0 {
			t.Errorf("%s: %v", filepath.Base(file), problems)
		}
	}
}

func TestGoVersion_WorkflowGuardNegativeControls(t *testing.T) {
	for _, tc := range []struct {
		name, steps string
		bad         bool
	}{
		{"setup", "      - uses: actions/setup-go@fixed\n      - run: bash scripts/ci/assert-go-version.sh\n      - run: go test ./...\n", false},
		{"missing", "      - uses: actions/setup-go@fixed\n      - run: go test ./...\n", true},
		{"late", "      - uses: actions/setup-go@fixed\n      - run: go test ./...\n      - run: bash scripts/ci/assert-go-version.sh\n", true},
		{"container", "      - run: bash scripts/release/linux-build-prereqs.sh\n      - run: bash scripts/ci/assert-go-version.sh\n", false},
		{"container_missing", "      - run: bash scripts/release/linux-build-prereqs.sh\n", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			bad := len(goJobGuardProblems("jobs:\n  job:\n    steps:\n"+tc.steps)) > 0
			if bad != tc.bad {
				t.Fatalf("bad=%v want %v", bad, tc.bad)
			}
		})
	}
}
