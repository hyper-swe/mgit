package libkrun

import (
	"crypto/sha256"
	"encoding/hex"
	"path/filepath"
	"strings"
	"testing"

	"github.com/hyper-swe/mgit/internal/sandboxd/backend/microvm"
)

// THE PRODUCTION PATH MUST FIT IN sun_path, NOT JUST THE TEST PATH.
//
// Every socket this backend binds lives under the per-sandbox state dir, and
// unix sockets cap the whole path at 104 bytes including the NUL. macOS hands
// every process a ~48-byte private TMPDIR, which is where the daemon's runtime
// dir goes when XDG_RUNTIME_DIR is unset — i.e. always, on a Mac.
//
// The backend's own tests all build their state dirs with shortTempDir(),
// which is exactly the workaround that kept this invisible: the real default
// exceeded the limit by 13 bytes and `mgit run` could not boot a VM on a stock
// Mac at all. This test uses the REAL derivation with a realistic macOS
// TMPDIR, so the margin is measured rather than assumed.
// Refs: MGIT-61.15, FR-17.13

// darwinTempDirLen is the length of the per-user temp directory macOS hands
// every process: /var/folders/<2>/<30>/T/ — fixed-width, so this is not an
// estimate. os.TempDir() returns it whenever TMPDIR is set, which launchd
// always does.
const darwinTempDirLen = len("/var/folders/q8/3chs9j9n3p9494fvnq3xblpr0000gn/T/")

func TestSocketPaths_FitUnderARealisticMacOSRuntimeDir(t *testing.T) {
	// The production derivation, mirrored: <tmp>/mgit-<uid>/<repo-key>/work,
	// then the per-sandbox state dir under it. A 7-digit uid is used because
	// macOS uids are commonly 501 but need not be.
	tmp := "/var/folders/q8/" + strings.Repeat("x", 30) + "/T"
	if len(tmp)+1 != darwinTempDirLen {
		t.Fatalf("test's temp dir model is %d bytes, want %d", len(tmp)+1, darwinTempDirLen)
	}
	sum := sha256.Sum256([]byte("/Users/someone/some/project"))
	runtimeDir := filepath.Join(tmp, "mgit-1234567", hex.EncodeToString(sum[:6]))
	stateDir := microvm.SandboxStateDir(filepath.Join(runtimeDir, "work"), "01KYQ3DE2YGQXF04CYEE2XQFFP")

	paths := map[string]string{
		"vsock exec":   vsockSocketPath(stateDir, microvm.GuestExecPort),
		"vsock notify": vsockSocketPath(stateDir, microvm.GuestNotifyPort),
		"net deny":     filepath.Join(stateDir, denySocketName),
		"net proxy":    filepath.Join(stateDir, proxySocketName),
	}
	for kind, path := range paths {
		t.Run(kind, func(t *testing.T) {
			if err := checkSocketPathLen(kind, path); err != nil {
				t.Errorf("the DEFAULT macOS layout cannot bind this socket: %v", err)
			}
			t.Logf("%s: %d bytes, %d to spare", kind, len(path), maxUnixSocketPath-len(path))
		})
	}
}

// TestSandboxStateDir_KeepsTheSandboxTraceable guards the other half of the
// trade: the state dir must stay short enough to bind sockets under, without
// becoming a name nobody can connect back to a sandbox.
func TestSandboxStateDir_KeepsTheSandboxTraceable(t *testing.T) {
	const id = "01KYQ3DE2YGQXF04CYEE2XQFFP"
	seg := filepath.Base(microvm.SandboxStateDir("/w", id))

	if len(seg) > 12 {
		t.Errorf("state dir segment %q is %d bytes; the sockets bound under it "+
			"do not fit sun_path on macOS", seg, len(seg))
	}
	if seg == "" || !containsSuffixOf(id, seg) {
		t.Errorf("state dir segment %q is not derived from the sandbox ID %q, so a "+
			"directory found on disk cannot be traced back to its sandbox", seg, id)
	}
}

// containsSuffixOf reports whether seg is a suffix of id — the property that
// makes a truncated directory name greppable back to the full ULID.
func containsSuffixOf(id, seg string) bool {
	return len(seg) <= len(id) && id[len(id)-len(seg):] == seg
}
