package packaging

import (
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestSandboxPostureLandKeepsFailureOutput(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Unix posture script")
	}
	root := repoRoot(t)
	lib := filepath.Join(root, "scripts", "e2e", "lib.sh")
	for _, success := range []bool{false, true} {
		command := "printf 'land refused: sync the task before committing\\n' >&2; exit 23"
		if success {
			command = "printf 'land completed\\n'; exit 0"
		}
		cmd := exec.Command("bash", "-c", `source "$1"; assert_ok_with_output 'guest commit lands' -- bash -c "$2"`, "test", lib, command) //nolint:gosec // G204: fixed test command and repository-owned helper
		out, err := cmd.CombinedOutput()
		if success {
			if err != nil || !strings.Contains(string(out), "ok: guest commit lands") {
				t.Fatalf("successful land assertion: %v\n%s", err, out)
			}
		} else if err == nil || !strings.Contains(string(out), "land refused: sync the task before committing") || !strings.Contains(string(out), "FAIL: guest commit lands") {
			t.Fatalf("failed land must remain failed and retain its diagnostic: %v\n%s", err, out)
		}
	}
	body, err := os.ReadFile(filepath.Join(root, "scripts", "e2e", "sandbox_posture.sh")) //nolint:gosec // G304: fixed repository-owned script read for assertion wiring
	if err != nil {
		t.Fatal(err)
	}
	for _, label := range []string{"sandbox land succeeds", "the guest's commit lands"} {
		if !strings.Contains(string(body), `assert_ok_with_output "`+label+`" -- mgit sandbox land --task SB-1`) {
			t.Errorf("land assertion %q does not retain its failed command output", label)
		}
	}
}
