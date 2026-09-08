//go:build cgo && !vzf && (darwin || (linux && libkrun))

package libkrun

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/hyper-swe/mgit/internal/execwire"
	"github.com/hyper-swe/mgit/internal/model"
	"github.com/hyper-swe/mgit/internal/sandboxd/backend/microvm"
	"github.com/hyper-swe/mgit/internal/sandboxd/guestexec"
)

// TestE2E_Libkrun_RealVM_ExecRunsAsTheIdentityAsked is the live proof of
// MGIT-151 on this backend: the REAL mgit-guest as PID 1 in a real libkrun
// microVM, driven over the production exec wire, runs a command as the
// identity the host asks for — the host's own uid/gid, which is what the
// guest tree it shares is owned by — and the command's OWN view says so:
// its uid and gid, the name the guest's passwd resolves for that uid, its
// HOME, and the owner of a file it writes there. Asked for nothing, the
// guest still reports it ran as root. Refs: MGIT-151, FR-17.11
func TestE2E_Libkrun_RealVM_ExecRunsAsTheIdentityAsked(t *testing.T) {
	requireRealVM(t)
	guestRoot := t.TempDir()
	for _, d := range append([]string{"sbin"}, guestBaseDirs...) {
		if err := os.MkdirAll(filepath.Join(guestRoot, d), 0o750); err != nil {
			t.Fatal(err)
		}
	}
	buildIntoGuest(t, guestRoot, guestInitPath, "./cmd/mgit-guest")
	buildIntoGuest(t, guestRoot, "sbin/idprobe", "./internal/sandboxd/backend/libkrun/testdata/idprobe")

	workDir := shortTempDir(t)
	const sandboxID = "identity"
	cfg := realVMConfig(t, guestRoot, model.NetworkModeNone, nil)
	cfg.SandboxID = sandboxID
	cfg.StateDir = microvm.SandboxStateDir(workDir, sandboxID)
	if err := os.MkdirAll(cfg.StateDir, 0o700); err != nil {
		t.Fatal(err)
	}
	cfg.RootfsReadOnly = false
	cfg.VsockEnabled = true
	vm, console := bootVMUntil(t, cfg, `"vsock_port":1024`)
	t.Cleanup(func() { _ = vm.Stop(context.Background(), true) })

	asked := model.GuestIdentity{UID: os.Getuid(), GID: os.Getgid(), Name: "agent", Home: "/home/agent"}
	out, res := execIdentityProbe(t, workDir, sandboxID, model.ExecRequest{Command: []string{"/sbin/idprobe"}, RunAs: &asked})
	want := fmt.Sprintf("uid=%d gid=%d name=agent home=/home/agent home_file_owner=%d:%d", asked.UID, asked.GID, asked.UID, asked.GID)
	if strings.TrimSpace(out) != want {
		t.Errorf("the command's own view of its identity:\n got %q\nwant %q\nconsole:\n%s", strings.TrimSpace(out), want, console)
	}
	if res.RanAs == nil || *res.RanAs != asked {
		t.Errorf("the guest echoes the identity it ran as: got %+v, want %+v", res.RanAs, asked)
	}

	// Nothing asked: the guest runs it as itself and SAYS so.
	out2, res2 := execIdentityProbe(t, workDir, sandboxID, model.ExecRequest{Command: []string{"/sbin/idprobe"}})
	if !strings.HasPrefix(out2, "uid=0 gid=0 ") {
		t.Errorf("with nothing asked the command runs as the guest's own root: %q", out2)
	}
	if res2.RanAs == nil || !res2.RanAs.IsRoot() {
		t.Errorf("the guest reports root when nothing was asked: %+v", res2.RanAs)
	}
	if !t.Failed() {
		t.Logf("IDENTITY REAL VM PASS: asked uid %d gid %d; the guest's child saw %q and the guest echoed %+v; nothing asked ran as root and said so",
			asked.UID, asked.GID, strings.TrimSpace(out), *res.RanAs)
	}
}

// buildIntoGuest cross-compiles pkg into the guest root at rel, static and
// reproducible, exactly as the guest tree ships its binaries.
func buildIntoGuest(t *testing.T, guestRoot, rel, pkg string) {
	t.Helper()
	//nolint:gosec // G204: fixed argv; the output path is under a t.TempDir
	build := exec.Command("go", "build", "-trimpath", "-buildvcs=false", "-ldflags=-buildid=",
		"-o", filepath.Join(guestRoot, rel), pkg)
	build.Dir = repoRoot(t)
	build.Env = append(os.Environ(), "GOOS=linux", "GOARCH="+runtime.GOARCH, "CGO_ENABLED=0")
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build %s: %v\n%s", pkg, err, out)
	}
}

// execIdentityProbe drives one exec through the production dialer and wire, and
// returns the command's combined output and the wire result.
func execIdentityProbe(t *testing.T, workDir, sandboxID string, req model.ExecRequest) (string, execwire.Result) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	conn, err := newGuestDialer(workDir).DialGuest(ctx, sandboxID)
	if err != nil {
		t.Fatalf("host could not reach mgit-guest's exec channel: %v", err)
	}
	defer func() { _ = conn.Close() }()
	var stdout, stderr bytes.Buffer
	res, err := guestexec.Run(conn, req, &stdout, &stderr)
	if err != nil {
		t.Fatalf("exec failed: %v (stderr=%q)", err, stderr.String())
	}
	return stdout.String() + stderr.String(), res
}
