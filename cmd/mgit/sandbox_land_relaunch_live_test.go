//go:build darwin || linux

package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// TestSandboxLand_RealGuestRelaunchAndDivergence covers a supported workflow
// and a separate, booted-guest divergence without relaxing ancestry checks.
// Refs: MGIT-317
func TestSandboxLand_RealGuestRelaunchAndDivergence(t *testing.T) {
	if os.Getenv("MGIT_E2E_LIBKRUN") != "1" {
		t.Skip("opt-in real guest")
	}
	t.Helper()
	_, source, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("locate source tree")
	}
	sourceRoot := filepath.Clean(filepath.Join(filepath.Dir(source), "..", ".."))
	binDir := t.TempDir()
	cli := filepath.Join(binDir, "mgit")
	daemon := filepath.Join(binDir, "mgit-sandboxd")
	guestDir := filepath.Join(binDir, "guest")
	if err := os.MkdirAll(guestDir, 0o700); err != nil {
		t.Fatal(err)
	}
	build := func(output, target string, guest bool) {
		t.Helper()
		args := []string{"build", "-buildvcs=false", "-o", output}
		if target == "./cmd/mgit-sandboxd" {
			args = append(args, "-tags", "libkrun")
		}
		args = append(args, target)
		cmd := exec.CommandContext(t.Context(), "go", args...) //nolint:gosec // G204: fixed Go build targets, test-owned outputs
		cmd.Dir = sourceRoot
		if target == "./cmd/mgit-sandboxd" {
			cmd.Env = append(os.Environ(), "CGO_ENABLED=1")
		}
		if guest {
			cmd.Env = append(os.Environ(), "GOOS=linux", "GOARCH="+runtime.GOARCH, "CGO_ENABLED=0")
		}
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("build %s: %v\n%s", target, err, out)
		}
	}
	build(cli, "./cmd/mgit", false)
	build(daemon, "./cmd/mgit-sandboxd", false)
	build(filepath.Join(guestDir, "mgit"), "./cmd/mgit", true)
	build(filepath.Join(guestDir, "mgit-guest"), "./cmd/mgit-guest", true)
	if runtime.GOOS == "darwin" {
		liveLandCommand(t, sourceRoot, "codesign", "--force", "--sign", "-", "--entitlements",
			filepath.Join(sourceRoot, "build", "darwin", "vz.entitlements"), daemon)
	}
	liveLandCommand(t, sourceRoot, cli, "--version")
	liveLandCommand(t, sourceRoot, daemon, "--version")

	fixture := t.TempDir()
	root, native := filepath.Join(fixture, "root"), filepath.Join(fixture, "root", "wt")
	if err := os.MkdirAll(root, 0o700); err != nil {
		t.Fatal(err)
	}
	liveLandCommand(t, root, "git", "init")
	liveLandCommand(t, root, "git", "config", "user.name", "HyperSWE agent")
	liveLandCommand(t, root, "git", "config", "user.email", "swe@hyperswe.com")
	liveLandCommand(t, root, "git", "commit", "--allow-empty", "-m", "test: seed guest land readback")
	liveLandCommand(t, root, cli, "init")
	liveLandCommand(t, root, cli, "sandbox", "image", "init")
	liveLandCommand(t, root, cli, "sandbox", "base", "from", "--guest-bin-dir", guestDir, "--json")
	const task = "SB-1"
	// Registered before launch, so even a failed first boot gets cleanup.
	t.Cleanup(func() {
		if paths, err := resolveSandboxPaths(root); err == nil {
			if data, err := os.ReadFile(paths.daemonLog); err == nil { //nolint:gosec // G304: this fixture's own daemon log
				for _, line := range strings.Split(string(data), "\n") {
					if strings.Contains(line, "notify_land_error") || strings.Contains(line, "notify_landed") {
						t.Logf("test daemon land event: %s", line)
					}
				}
			}
		}
		if _, err := liveLandCommandResult(t, root, cli, "sandbox", "remove", task, "--force"); err != nil {
			t.Errorf("remove test sandbox: %v", err)
		}
		if _, err := liveLandCommandResult(t, root, cli, "sandbox", "daemons", "stop", "--repo-root", root); err != nil {
			t.Errorf("stop test daemon: %v", err)
		}
		if pids := liveLandDaemonPIDs(t, daemon, root); len(pids) != 0 {
			t.Errorf("throwaway daemon still alive after cleanup: %v", pids)
		}
		if _, err := liveLandCommandResult(t, root, cli, "worktree", "remove", "wt", "--force"); err != nil {
			t.Errorf("remove test worktree: %v", err)
		}
	})
	liveLandCommand(t, root, cli, "work", "wt", "--task-id", task, "--sandbox", "--memory-mb", "512", "--cpus", "1", "--network", "none")
	liveLandSnapshot(t, "before host commit: root", root)
	liveLandSnapshot(t, "before guest: native", native)
	liveLandCommand(t, native, cli, "sandbox", "exec", "--task-id", task, "--timeout", "90s", "--",
		"sh", "-ec", "pwd; mgit --version")
	if pids := liveLandDaemonPIDs(t, daemon, root); len(pids) != 1 {
		t.Fatalf("expected one captured test-built daemon identity, got %v", pids)
	}
	liveLandCommand(t, native, cli, "sandbox", "status", task, "--json")

	liveLandCommand(t, native, "sh", "-ec", "printf 'contained\n' > built.txt")
	liveLandCommand(t, native, cli, "add", ".")
	liveLandCommand(t, native, cli, "commit", "-m", "work in sandbox")
	liveLandCommand(t, native, cli, "log", "--task-id", task, "--json")
	out, err := liveLandCommandResult(t, root, cli, "sandbox", "land", "--task", task)
	if err != nil {
		t.Fatalf("CI host-native commit -> land refused: %v\n%s", err, out)
	}
	liveLandCommand(t, root, cli, "sandbox", "remove", task, "--force")
	liveLandCommand(t, root, cli, "sandbox", "launch", "--task-id", task, "--worktree", native,
		"--memory-mb", "512", "--cpus", "1", "--network", "none", "--json")
	liveLandCommand(t, native, cli, "run", "--", "sh", "-c", "printf 'from the guest\\n' > guest-step.txt && mgit commit -a -m 'guest step, no task id'")
	out, err = liveLandCommandResult(t, root, cli, "sandbox", "land", "--task", task)
	if err != nil {
		t.Fatalf("relaunch from current native tip did not deliver guest commit: %v\n%s", err, out)
	}
	data, readErr := os.ReadFile(filepath.Join(native, "guest-step.txt")) //nolint:gosec // G304: fixed file inside the test-owned native fixture
	if readErr != nil || string(data) != "from the guest\n" {
		t.Fatalf("guest bytes missing from native: %v %q", readErr, data)
	}
	if _, readErr := os.Stat(filepath.Join(root, "guest-step.txt")); !os.IsNotExist(readErr) {
		t.Fatalf("guest file reached root: %v", readErr)
	}
	// A fresh registration avoids reusing delivery bookkeeping from the
	// preceding landed guest commit. The next case isolates ancestry refusal.
	liveLandCommand(t, root, cli, "sandbox", "remove", task, "--force")
	liveLandCommand(t, root, cli, "sandbox", "launch", "--task-id", task, "--worktree", native,
		"--memory-mb", "512", "--cpus", "1", "--network", "none", "--json")
	// Launch is lazy: force the guest to boot before the native tip moves.
	liveLandCommand(t, native, cli, "sandbox", "exec", "--task-id", task, "--timeout", "90s", "--", "sh", "-ec", "pwd; mgit --version")
	liveLandCommand(t, native, "sh", "-ec", "printf 'host advanced again\\n' > built.txt")
	liveLandCommand(t, native, cli, "add", "--", "built.txt")
	liveLandCommand(t, native, cli, "commit", "-m", "test: advance native with guest still booted")
	liveLandCommand(t, native, cli, "run", "--", "sh", "-c", "printf 'must remain in ledger\\n' > refused-step.txt && mgit commit -a -m 'test: divergent guest step'")
	out, err = liveLandCommandResult(t, root, cli, "sandbox", "land", "--task", task)
	liveLandCommand(t, native, cli, "log", "--task-id", task, "--json")
	liveLandCommand(t, native, "sh", "-c", "printf 'native built: '; cat built.txt; printf 'native guest: '; cat guest-step.txt")
	if err == nil {
		t.Fatal("divergent land unexpectedly succeeded")
	}
	for _, want := range []string{"non-fast-forward", "host tip ", "guest base ", "guest commit ", "is retained in task ledger", "no automatic recovery yet: MGIT-319"} {
		if !strings.Contains(out, want) {
			t.Fatalf("missing refusal detail %q in %s", want, out)
		}
	}
	data, readErr = os.ReadFile(filepath.Join(native, "built.txt")) //nolint:gosec // G304: fixed file inside the test-owned native fixture
	if readErr != nil || string(data) != "host advanced again\n" {
		t.Fatalf("native host commit changed: %v %q", readErr, data)
	}
	for _, target := range []string{root, native} {
		if _, readErr := os.Stat(filepath.Join(target, "refused-step.txt")); !os.IsNotExist(readErr) {
			t.Fatalf("refused bytes reached %s: %v", target, readErr)
		}
	}
}
