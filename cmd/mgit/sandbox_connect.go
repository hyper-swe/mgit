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
	gitstore "github.com/hyper-swe/mgit/internal/store/git"
)

// sandboxPaths locates the daemon socket plus the host config root and
// sandbox-local work dir for one repository.
type sandboxPaths struct {
	socket    string // unix socket the daemon serves (kept short for sun_path)
	daemonLog string // capture of the spawned daemon's output, for diagnosing a failed activation
	hostRoot  string // durable host config: images.lock, trust root, policy, audit
	workDir   string // ephemeral sandbox-local state; never a worktree
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
		socket:    filepath.Join(runtimeDir, "d.sock"),
		daemonLog: filepath.Join(runtimeDir, daemonLogName),
		hostRoot:  filepath.Join(repoRoot, ".mgit", "sandbox"),
		// "w", not "work": the per-sandbox socket paths under this directory
		// share a 104-byte sun_path budget with a 48-byte macOS TMPDIR.
		// Refs: MGIT-61.15
		workDir: filepath.Join(runtimeDir, "w"),
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

// sandboxRepoRoot resolves the repo root that OWNS the sandbox daemon for a
// working directory: the nearest ancestor with a .mgit directory, and — when
// that ancestor is a linked worktree (its .mgit carries a marker, not the
// store) — the shared parent repo the marker points at. The daemon socket
// key, host root (images/policy/audit), and --repo-root must all be the
// parent's; sandbox-to-worktree routing is handled by the sandbox records'
// WorktreePath matching, not by per-worktree daemons. Refs: MGIT-57
func sandboxRepoRoot(cwd string) (string, error) {
	root, err := findRepoRoot(cwd)
	if err != nil {
		return "", err
	}
	marker, isWorktree, err := gitstore.ReadWorktreeMarker(root)
	if err != nil {
		return "", fmt.Errorf("read worktree marker: %w", err)
	}
	if isWorktree {
		return filepath.Dir(marker.Store), nil
	}
	return root, nil
}

// locateSandboxd finds the mgit-sandboxd binary: first alongside this
// executable (the normal install layout), then on PATH.
func locateSandboxd() (string, error) {
	exe, err := os.Executable()
	if err != nil {
		exe = ""
	}
	return locateSandboxdFor(exe)
}

// locateSandboxdFor finds the daemon relative to a given mgit binary, then on
// PATH. exePath is taken as a parameter so the lookup is testable without
// being the test binary's own path.
//
// Symlinks are resolved first: the ordinary way to install from an archive is
// to extract it and symlink mgit into a PATH directory, and macOS reports the
// SYMLINK as the executable — so "beside my own binary" would resolve to a
// directory holding no daemon. The two binaries ship together so they can find
// each other; that has to survive being put on PATH. Refs: MGIT-65, MGIT-44
func locateSandboxdFor(exePath string) (string, error) {
	if exePath != "" {
		if resolved, err := filepath.EvalSymlinks(exePath); err == nil {
			exePath = resolved
		}
		cand := filepath.Join(filepath.Dir(exePath), "mgit-sandboxd")
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
	return sandboxConnectFor(ctx, cwd)
}

// sandboxConnectFor resolves the daemon owning the repo that contains dir.
//
// The directory is a parameter rather than always the process cwd because a
// long-lived server can be started from anywhere and told which project to
// serve (`mgit serve --project`): keying the daemon on ITS cwd would address a
// different repo's sandboxes, or none. Refs: MGIT-57, MGIT-60, MGIT-76
func sandboxConnectFor(ctx context.Context, dir string) (sandboxClient, error) {
	// Resolve the OWNING repo, not the raw directory: from inside a linked
	// worktree (whose .mgit is a directory holding only the marker + shims) the
	// sandbox daemon, socket key, and host root all belong to the SHARED PARENT
	// repo — keying them on the worktree spawned a second daemon against a
	// nonexistent host root, which died, breaking `mgit run` inside the very
	// worktree `mgit work --sandbox` creates. Refs: MGIT-57
	repoRoot, err := sandboxRepoRoot(dir)
	if err != nil {
		return nil, err
	}
	p, err := resolveSandboxPaths(repoRoot)
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
		c := exec.Command(bin, "--socket", p.socket, "--host-root", p.hostRoot,
			"--repo-root", repoRoot, "--work-dir", p.workDir)
		// Capture what the daemon says. It is detached into its own session,
		// so without this its explanation for dying — including the dynamic
		// loader's, which is emitted before the daemon's own code runs — goes
		// nowhere and every failure looks identical. Truncated per attempt so
		// the tail always describes THIS spawn. Refs: MGIT-61.14, MGIT-61.15
		if logFile, lerr := os.OpenFile(p.daemonLog, //nolint:gosec // a path this process derived, owner-only dir
			os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600); lerr == nil {
			defer func() { _ = logFile.Close() }()
			c.Stdout, c.Stderr = logFile, logFile
		}
		configureDaemonCmd(c) // detach into its own session (platform-guarded)
		return c.Start()
	}
	if err := sandboxd.EnsureRunning(ctx, p.socket, spawn); err != nil {
		return nil, fmt.Errorf(
			"sandbox daemon unavailable (no fallback — task work runs only inside the sandbox): %w%s",
			err, daemonFailureDetail(p.daemonLog))
	}
	return sandboxd.NewClient(p.socket, func() time.Time { return time.Now().UTC() }), nil
}
