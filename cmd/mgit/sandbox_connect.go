package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"time"

	"github.com/hyper-swe/mgit/internal/sandboxd"
)

// sandboxPaths locates the daemon socket plus the host config root and
// sandbox-local work dir for one repository.
type sandboxPaths struct {
	socket   string // unix socket the daemon serves (kept short for sun_path)
	hostRoot string // durable host config: images.lock, trust root, policy, audit
	workDir  string // ephemeral sandbox-local state; never a worktree
}

// resolveSandboxPaths derives the per-repo sandbox paths. Durable host
// config lives under the repo's .mgit; the socket and work dir live in a
// short, owner-only (0700) runtime dir keyed by repo path — short because
// the unix socket path is length-limited (~104 bytes), and owner-only so a
// foreign user cannot interpose a squatter socket beneath it.
// Refs: FR-17.13, FR-17.34, MGIT-11.10.9
func resolveSandboxPaths(repoRoot string) (sandboxPaths, error) {
	if fi, err := os.Stat(filepath.Join(repoRoot, ".mgit")); err != nil || !fi.IsDir() {
		return sandboxPaths{}, fmt.Errorf("not an mgit repository (no .mgit in %s)", repoRoot)
	}
	sum := sha256.Sum256([]byte(repoRoot))
	key := hex.EncodeToString(sum[:6])
	runtimeDir := filepath.Join(runtimeBase(), fmt.Sprintf("mgit-%d", os.Getuid()), key)
	if err := os.MkdirAll(runtimeDir, 0o700); err != nil {
		return sandboxPaths{}, fmt.Errorf("create sandbox runtime dir: %w", err)
	}
	return sandboxPaths{
		socket:   filepath.Join(runtimeDir, "d.sock"),
		hostRoot: filepath.Join(repoRoot, ".mgit", "sandbox"),
		workDir:  filepath.Join(runtimeDir, "work"),
	}, nil
}

// runtimeBase is the short base for ephemeral sandbox runtime state:
// XDG_RUNTIME_DIR when set (per-user, tmpfs), else the system temp dir.
func runtimeBase() string {
	if d := os.Getenv("XDG_RUNTIME_DIR"); d != "" {
		return d
	}
	return os.TempDir()
}

// locateSandboxd finds the mgit-sandboxd binary: first alongside this
// executable (the normal install layout), then on PATH.
func locateSandboxd() (string, error) {
	if exe, err := os.Executable(); err == nil {
		cand := filepath.Join(filepath.Dir(exe), "mgit-sandboxd")
		if _, err := os.Stat(cand); err == nil {
			return cand, nil
		}
	}
	if p, err := exec.LookPath("mgit-sandboxd"); err == nil {
		return p, nil
	}
	return "", fmt.Errorf("mgit-sandboxd binary not found (install it alongside mgit or on PATH)")
}

// productionSandboxConnect resolves the repo's daemon, activating it if
// needed (the greeting check rejects a squatter socket), and returns a
// client. An unavailable backend is a clear error with NO fallback —
// running task work outside the sandbox would defeat FR-17 containment.
// Refs: FR-17.34, NFR-17.6, MGIT-11.10.9
func productionSandboxConnect(ctx context.Context) (sandboxClient, error) {
	cwd, err := os.Getwd()
	if err != nil {
		return nil, fmt.Errorf("get working directory: %w", err)
	}
	p, err := resolveSandboxPaths(cwd)
	if err != nil {
		return nil, err
	}
	spawn := func() error {
		bin, lerr := locateSandboxd()
		if lerr != nil {
			return lerr
		}
		// NOT CommandContext: the daemon must OUTLIVE this CLI invocation
		// (it serves later commands and idle-exits on its own). Binding it
		// to ctx would kill it the moment this command returns.
		//nolint:gosec,noctx // fixed binary + derived owner-only paths, no shell; long-lived daemon must not die with the request ctx
		c := exec.Command(bin, "--socket", p.socket, "--host-root", p.hostRoot, "--work-dir", p.workDir)
		configureDaemonCmd(c) // detach into its own session (platform-guarded)
		return c.Start()
	}
	if err := sandboxd.EnsureRunning(ctx, p.socket, spawn); err != nil {
		return nil, fmt.Errorf("sandbox daemon unavailable (no fallback — task work runs only inside the sandbox): %w", err)
	}
	return sandboxd.NewClient(p.socket, func() time.Time { return time.Now().UTC() }), nil
}
