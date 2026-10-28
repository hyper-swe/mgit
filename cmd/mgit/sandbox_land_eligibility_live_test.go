//go:build darwin || linux

package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

// TestSandboxLand_RealGuestNotifyRefusedNotExported exercises automatic land
// after a routed guest commit, without a manual land call. Refs: MGIT-323

func TestSandboxLand_RealGuestNotifyRefusedNotExported(t *testing.T) {
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

	// The guest is already booted. Host work advances the trusted native tip,
	// while its private history remains at the earlier base.
	liveLandCommand(t, native, "sh", "-ec", "printf 'accepted host bytes\\n' > built.txt")
	liveLandCommand(t, native, cli, "add", "--", "built.txt")
	liveLandCommand(t, native, cli, "commit", "-m", "test: accepted host work")
	output := liveLandCommand(t, native, cli, "run", "--", "sh", "-ec",
		"printf 'ROUTED_GUEST_OS='; uname -s; printf 'refused guest bytes\\n' > refused-step.txt; mgit commit -a -m 'test: divergent guest work'")
	if !strings.Contains(output, "ROUTED_GUEST_OS=Linux") {
		t.Fatalf("routed command did not prove a Linux guest: %s", output)
	}
	// Only the automatic notify handler can have attempted land in this test.
	paths, err := resolveSandboxPaths(root)
	if err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(20 * time.Second)
	var refusal string
	for time.Now().Before(deadline) {
		data, readErr := os.ReadFile(paths.daemonLog) //nolint:gosec // G304: this fixture's own daemon log
		if readErr != nil {
			t.Fatal(readErr)
		}
		for _, line := range strings.Split(string(data), "\n") {
			if strings.Contains(line, "notify_land_error") && strings.Contains(line, "non-fast-forward") {
				refusal = line
				break
			}
		}
		if refusal != "" {
			break
		}
		time.Sleep(25 * time.Millisecond)
	}
	if refusal == "" {
		t.Fatal("automatic notify did not report the expected divergence refusal")
	}
	t.Logf("automatic refusal: %s", refusal)
	liveLandCommand(t, native, cli, "log", "--task-id", task, "--json")
	for _, target := range []string{root, native} {
		if _, statErr := os.Stat(filepath.Join(target, "refused-step.txt")); !os.IsNotExist(statErr) {
			t.Fatalf("refused guest bytes reached %s: %v", target, statErr)
		}
	}
	accepted, err := os.ReadFile(filepath.Join(native, "built.txt")) //nolint:gosec // G304: fixed path in the test-owned native fixture
	if err != nil || string(accepted) != "accepted host bytes\n" {
		t.Fatalf("accepted host content changed: %v %q", err, accepted)
	}
	patch := liveLandCommand(t, root, cli, "squash", "--task-id", task, "--to-git")
	if !strings.Contains(patch, "built.txt") || !strings.Contains(patch, "accepted host bytes") {
		t.Fatalf("accepted host positive control missing from export: %s", patch)
	}
	if strings.Contains(patch, "refused-step.txt") || strings.Contains(patch, "refused guest bytes") {
		t.Fatalf("automatically refused guest commit exported:\n%s", patch)
	}
}
