package packaging

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// coverFixture writes a small module whose internal/ holds one passing and
// one failing package, and returns its directory.
func coverFixture(t *testing.T, failing bool) string {
	t.Helper()
	dir := t.TempDir()
	write := func(rel, body string) {
		p := filepath.Join(dir, filepath.FromSlash(rel))
		require.NoError(t, os.MkdirAll(filepath.Dir(p), 0o750))
		require.NoError(t, os.WriteFile(p, []byte(body), 0o600))
	}
	write("go.mod", "module example.com/covfix\n\ngo 1.21\n")
	write("internal/good/good.go", "package good\n\nfunc One() int { return 1 }\n")
	write("internal/good/good_test.go", "package good\n\nimport \"testing\"\n\nfunc TestOne(t *testing.T) {\n\tif One() != 1 {\n\t\tt.Fatal(\"one\")\n\t}\n}\n")
	want := "1"
	if failing {
		want = "2"
	}
	write("internal/bad/bad.go", "package bad\n\nfunc One() int { return 1 }\n")
	write("internal/bad/bad_test.go", "package bad\n\nimport \"testing\"\n\nfunc TestOne(t *testing.T) {\n\tif One() != "+want+" {\n\t\tt.Fatal(\"bad package fails here\")\n\t}\n}\n")
	return dir
}

// runTestCover runs the repository's own `make test-cover` recipe inside dir.
func runTestCover(t *testing.T, dir string) (string, error) {
	t.Helper()
	if _, err := exec.LookPath("make"); err != nil {
		t.Skip("make is not installed")
	}
	//nolint:gosec // G204: test-only; the arguments are fixed and the Makefile is this repository's own
	cmd := exec.Command("make", "-s", "-f", filepath.Join(repoRoot(t), "Makefile"), "test-cover")
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	return string(out), err
}

// A failing package must fail `make test-cover`, name the package, and still
// leave the passing package's coverage in the profile. Before this was pinned
// the target ran every package, threw each exit status away and printed a
// coverage total, so CI's coverage step stayed green over a red package.
// Refs: MGIT-329
func TestMakeTestCover_AFailingPackage_FailsTheTargetAndNamesIt(t *testing.T) {
	dir := coverFixture(t, true)
	out, err := runTestCover(t, dir)
	require.Error(t, err, "a failing package must fail the target; output:\n%s", out)
	assert.Contains(t, out, "example.com/covfix/internal/bad", "the failing package must be named")
	prof, rerr := os.ReadFile(filepath.Join(dir, "cover.out")) //nolint:gosec // G304: test-only path under t.TempDir
	require.NoError(t, rerr)
	assert.Contains(t, string(prof), "internal/good/good.go", "the passing package is still measured")
}

func TestMakeTestCover_AllPackagesPass_SucceedsAndPrintsTheTotal(t *testing.T) {
	dir := coverFixture(t, false)
	out, err := runTestCover(t, dir)
	require.NoError(t, err, "output:\n%s", out)
	assert.True(t, strings.Contains(out, "total:"), "the coverage total is printed; output:\n%s", out)
}

// The Test job runs the whole suite, and a test in internal/store/git walks
// this repository's tracked files, which the product refuses to do in a
// shallow clone. The job therefore checks out the full history. Refs: MGIT-329
func TestCIWorkflow_TestJobChecksOutFullHistory(t *testing.T) {
	ci := readRepoFile(t, ".github/workflows/ci.yml")
	start := strings.Index(ci, "\n  test:\n")
	require.GreaterOrEqual(t, start, 0, "the test job must exist")
	end := strings.Index(ci[start:], "actions/setup-go@")
	require.Greater(t, end, 0, "the checkout must come before setup-go")
	assert.Contains(t, ci[start:start+end], "fetch-depth: 0", "the Test job's checkout must not be shallow")
}
