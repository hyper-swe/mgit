//go:build darwin || linux

package main

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"
	"time"
)

// TestSandboxLand_RealGuestNativeReadback exercises the real CLI and daemon,
// not a fake land client. It is deliberately opt-in: the caller must satisfy
// the host's real-VM resource/prerequisite checks before invoking it.
// Refs: MGIT-317
func TestSandboxLand_RealGuestNativeReadback(t *testing.T) {
	if os.Getenv("MGIT_E2E_LIBKRUN") != "1" {
		t.Skip("SKIP real guest land: set MGIT_E2E_LIBKRUN=1 on a prepared libkrun host; check resources first")
	}
	if runtime.GOOS == "darwin" && runtime.GOARCH != "arm64" {
		t.Skip("SKIP real guest land: macOS requires Apple Silicon")
	}
	for _, hostSync := range []bool{false, true} {
		name := "guest-only"
		if hostSync {
			name = "host-sync"
		}
		t.Run(name, func(t *testing.T) { liveLandScenario(t, hostSync) })
	}
}

func liveLandScenario(t *testing.T, hostSync bool) {
	t.Helper()
	_, source, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("locate the source tree")
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
	root, native := filepath.Join(fixture, "root"), filepath.Join(fixture, "native")
	registration := native
	if hostSync {
		// Match the documented `mgit work wt` / Linux live posture path:
		// registry relative, host sandbox binding canonical and absolute.
		native = filepath.Join(root, "wt")
		registration = "wt"
	}
	if err := os.MkdirAll(filepath.Join(root, "src"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "src", "existing.txt"), []byte("baseline existing\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	liveLandCommand(t, root, "git", "init")
	liveLandCommand(t, root, "git", "config", "user.name", "HyperSWE agent")
	liveLandCommand(t, root, "git", "config", "user.email", "swe@hyperswe.com")
	liveLandCommand(t, root, "git", "add", "--", "src/existing.txt")
	liveLandCommand(t, root, "git", "commit", "-m", "test: seed guest land readback")
	liveLandCommand(t, root, cli, "init", "--no-agent-files")
	liveLandCommand(t, root, cli, "sandbox", "image", "init")
	liveLandCommand(t, root, cli, "sandbox", "base", "from", "--guest-bin-dir", guestDir, "--json")
	const task = "MGIT-317"
	liveLandCommand(t, root, cli, "worktree", "add", registration, "--task-id", task, "--agent-id", "land-regression")
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
		if _, err := liveLandCommandResult(t, root, cli, "worktree", "remove", registration, "--force"); err != nil {
			t.Errorf("remove test worktree: %v", err)
		}
	})
	rootBefore := liveLandSnapshot(t, "before guest: root", root)
	liveLandSnapshot(t, "before guest: native", native)
	liveLandCommand(t, root, cli, "sandbox", "launch", "--task-id", task, "--worktree", native,
		"--memory-mb", "512", "--cpus", "1", "--network", "none", "--json")
	liveLandCommand(t, native, cli, "sandbox", "exec", "--task-id", task, "--timeout", "90s", "--",
		"sh", "-ec", "pwd; mgit --version")
	if pids := liveLandDaemonPIDs(t, daemon, root); len(pids) != 1 {
		t.Fatalf("expected one captured test-built daemon identity, got %v", pids)
	}
	liveLandCommand(t, native, cli, "sandbox", "status", task, "--json")
	guestScript := "mkdir -p src docs; printf 'guest changed existing\\n' > src/existing.txt; " +
		"printf 'guest created source\\n' > src/new.txt; printf 'guest created documentation\\n' > docs/new.txt; "
	if hostSync {
		for path, data := range liveLandExpected() {
			if err := os.MkdirAll(filepath.Dir(filepath.Join(native, path)), 0700); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(native, path), []byte(data), 0600); err != nil {
				t.Fatal(err)
			}
		}
		liveLandSnapshot(t, "host edits before sync: native", native)
		liveLandCommand(t, native, cli, "sandbox", "sync", "--task-id", task, "--json")
		// No guest-side writes: only the actual sync can supply these bytes.
		guestScript = "test \"$(cat src/existing.txt)\" = 'guest changed existing'; test \"$(cat src/new.txt)\" = 'guest created source'; test \"$(cat docs/new.txt)\" = 'guest created documentation'; "
	}
	guest := liveLandCommand(t, native, cli, "sandbox", "exec", "--task-id", task, "--timeout", "90s", "--", "sh", "-ec",
		guestScript+"mgit add -- src/existing.txt src/new.txt docs/new.txt; mgit commit --task-id MGIT-317 --agent-id land-regression -m 'test: guest native delivery' --json")
	liveLandSnapshot(t, "after guest commit, before host log: root", root)
	liveLandSnapshot(t, "after guest commit, before host log: native", native)
	committedID := liveLandWaitAttested(t, native, cli, task, guest)
	liveLandSnapshot(t, "after commit, before land: root", root)
	liveLandSnapshot(t, "after commit, before land: native", native)
	land := liveLandCommand(t, native, cli, "sandbox", "land", "--task-id", task, "--json")
	// Read BOTH views immediately, before cleanup or any other CLI operation.
	rootAfter := liveLandSnapshot(t, "immediately after land: root", root)
	for path := range liveLandExpected() {
		if rootBefore[path] != rootAfter[path] {
			t.Errorf("land changed repository-root path %s instead of only the native worktree", path)
		}
	}
	got := liveLandSnapshot(t, "immediately after land: native", native)
	for path, expected := range liveLandExpected() {
		if got[path] != expected {
			t.Errorf("land succeeded (%s), but attested guest commit %s is missing from native %s: got %q, want %q",
				strings.TrimSpace(land), committedID, path, got[path], expected)
		}
	}

	if hostSync {
		const hostConflict = "conflicting host edit\n"
		if err := os.WriteFile(filepath.Join(native, "src", "existing.txt"), []byte(hostConflict), 0600); err != nil {
			t.Fatal(err)
		}
		third := liveLandCommand(t, native, cli, "sandbox", "exec", "--task-id", task, "--timeout", "90s", "--", "sh", "-ec",
			"printf 'guest recovery tip\\n' > src/existing.txt; chmod 700 src/existing.txt; mgit add -- src/existing.txt; mgit commit --task-id MGIT-317 --agent-id land-regression -m 'test: recover pending projection' --json")
		_ = liveLandWaitAttested(t, native, cli, task, third)
		refusal, err := liveLandCommandResult(t, native, cli, "sandbox", "land", "--task-id", task, "--json")
		if err == nil || !strings.Contains(refusal, "native projection would overwrite") || !strings.Contains(refusal, "mgit restore") {
			t.Fatalf("expected actionable pending-projection conflict, got err=%v %s", err, refusal)
		}
		data, err := os.ReadFile(filepath.Join(native, "src", "existing.txt")) //nolint:gosec // G304: fixed path in this test-owned native fixture
		if err != nil {
			t.Fatal(err)
		}
		if string(data) != hostConflict {
			t.Fatalf("refused projection changed host bytes: %q", data)
		}
		if err := os.Rename(filepath.Join(native, "src", "existing.txt"), filepath.Join(fixture, "conflicting-host-backup.txt")); err != nil {
			t.Fatal(err)
		}
		// Follow the refusal's runnable remedy after preserving the host version.
		if !strings.Contains(refusal, "mgit restore './src/existing.txt' --commit "+committedID) {
			t.Fatalf("refusal must name admitted native checkpoint: %s", refusal)
		}
		liveLandCommand(t, native, cli, "restore", "./src/existing.txt", "--commit", committedID)
		liveLandCommand(t, native, cli, "restore", "--staged", "src/existing.txt")
		liveLandCommand(t, native, cli, "sandbox", "land", "--task-id", task, "--json")
		recovered := liveLandSnapshot(t, "after pending-projection recovery: native", native)
		if recovered["src/existing.txt"] != "guest recovery tip\n" {
			t.Fatalf("recovery did not publish attested tip: %q", recovered)
		}
		mode, err := os.Stat(filepath.Join(native, "src", "existing.txt"))
		if err != nil || mode.Mode().Perm()&0111 == 0 {
			t.Fatalf("land did not restore executable guest mode: mode=%v err=%v", mode, err)
		}
		backup, err := os.ReadFile(filepath.Join(fixture, "conflicting-host-backup.txt")) //nolint:gosec // G304: fixed backup in this test-owned fixture
		if err != nil || string(backup) != hostConflict {
			t.Fatalf("host backup lost: %q %v", backup, err)
		}
		finalRoot := liveLandSnapshot(t, "after pending-projection recovery: root", root)
		for path := range liveLandExpected() {
			if finalRoot[path] != rootBefore[path] {
				t.Errorf("recovery changed root path %s", path)
			}
		}
	}
}

func liveLandWaitAttested(t *testing.T, native, cli, task, guest string) string {
	t.Helper()
	var committed struct {
		ID string `json:"commit_id"`
	}
	for _, line := range strings.Split(guest, "\n") {
		if strings.HasPrefix(line, "{") {
			if err := json.Unmarshal([]byte(line), &committed); err != nil {
				t.Fatal(err)
			}
		}
	}
	if committed.ID == "" {
		t.Fatal("guest commit did not report an ID")
	}
	var host struct {
		Commits []struct {
			ID      string `json:"commit_hash"`
			Sandbox string `json:"sandbox_id"`
		} `json:"commits"`
	}
	// The commit notification is asynchronous. Wait for host attestation, not
	// for file projection: the latter is the behavior the land assertion tests.
	attested := false
	deadline := time.Now().Add(15 * time.Second)
	for !attested && time.Now().Before(deadline) {
		history := liveLandCommand(t, native, cli, "log", "--task-id", task, "--json")
		if err := json.Unmarshal([]byte(history), &host); err != nil {
			t.Fatal(err)
		}
		for _, c := range host.Commits {
			attested = attested || c.ID == committed.ID && c.Sandbox != ""
		}
		if !attested {
			time.Sleep(100 * time.Millisecond)
		}
	}
	if !attested {
		t.Fatal("precondition: the guest commit must already be attested in the host log")
	}

	return committed.ID
}

func liveLandExpected() map[string]string {
	return map[string]string{"src/existing.txt": "guest changed existing\n", "src/new.txt": "guest created source\n", "docs/new.txt": "guest created documentation\n"}
}

func liveLandSnapshot(t *testing.T, phase, dir string) map[string]string {
	t.Helper()
	out := make(map[string]string)
	for path := range liveLandExpected() {
		data, err := os.ReadFile(filepath.Join(dir, path)) //nolint:gosec // G304: fixed paths in this test's own fixture
		if err != nil && !os.IsNotExist(err) {
			t.Fatal(err)
		}
		if err == nil {
			out[path] = string(data)
		}
		if err == nil {
			t.Logf("readback %s %s exists=true sha256=%x", phase, path, sha256.Sum256(data))
		} else {
			t.Logf("readback %s %s exists=false sha256=-", phase, path)
		}
	}
	return out
}

func liveLandCommand(t *testing.T, dir, binary string, args ...string) string {
	t.Helper()
	out, err := liveLandCommandResult(t, dir, binary, args...)
	if err != nil {
		t.Fatalf("command failed: %v", err)
	}
	return out
}

func liveLandCommandResult(t *testing.T, dir, binary string, args ...string) (string, error) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	cmd := exec.CommandContext(ctx, binary, args...) //nolint:gosec // G204: test-built binaries or fixed system utilities, test-owned arguments
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	t.Logf("command cwd=%s argv=%q exit=%v\n%s", dir, append([]string{binary}, args...), err, out)
	return string(out), err
}

// Capture the actual process identity while the guest is running; do not
// infer it solely from a sibling daemon's version string. Never print other
// processes' arguments, which may hold unrelated credentials.
func liveLandDaemonPIDs(t *testing.T, daemon, root string) []int {
	t.Helper()
	resolved, err := filepath.EvalSymlinks(daemon)
	if err != nil {
		t.Fatal(err)
	}
	rootResolved, err := filepath.EvalSymlinks(root)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, "ps", "-axww", "-o", "pid=,command=").Output()
	if err != nil {
		t.Fatal(err)
	}
	var pids []int
	for _, line := range strings.Split(string(out), "\n") {
		fields := strings.Fields(line)
		if len(fields) < 2 {
			continue
		}
		command := strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(line), fields[0]))
		if (!strings.HasPrefix(command, daemon+" ") && !strings.HasPrefix(command, resolved+" ")) ||
			!strings.Contains(command, "--repo-root") || (!strings.Contains(command, root) && !strings.Contains(command, rootResolved)) {
			continue
		}
		pid, err := strconv.Atoi(fields[0])
		if err != nil {
			t.Fatal(err)
		}
		data, err := os.ReadFile(resolved) //nolint:gosec // G304: resolved test-built daemon
		if err != nil {
			t.Fatal(err)
		}
		t.Logf("runtime daemon pid=%d executable=%s sha256=%x repo=%s", pid, resolved, sha256.Sum256(data), rootResolved)
		pids = append(pids, pid)
	}
	return pids
}
