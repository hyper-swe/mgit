//go:build cgo && !vzf && (darwin || (linux && libkrun))

package libkrun

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// A worktree is mounted in the guest at its IDENTICAL host path, so the guest
// must first create that path as a mount point in its own root. On Linux
// libkrun the guest root cannot copy up a directory the image ships (virtio-fs
// answers FS_IOC_GETFLAGS with EOPNOTSUPP, MGIT-89), so a mount point beneath
// one — /home/<user>/…, /var/…, /opt/… — could not be created, and every
// guest launched for a worktree there died at boot. Every other real-VM test
// keeps its worktree under /tmp (a tmpfs in the guest) or under a directory
// the image does not ship, which is why none of them saw it; a user's
// worktree in their home directory hit it on the first exec.
//
// This test puts the worktree under /var/tmp on the host. The guest root
// ships /var (buildGuestSupervisor gives it the FHS directories a real base
// has) and not /var/tmp, so creating the mount point means writing into the
// image's /var. On macOS, where copy-up works, the same test is the positive
// control. Refs: MGIT-230.7, MGIT-89
func TestE2E_Libkrun_RealVM_WorktreeUnderAnImageDirectory_IsMountedAtItsPath(t *testing.T) {
	requireRealVM(t)
	parent, err := os.MkdirTemp("/var/tmp", "mgit-e2e-wtpath-")
	if err != nil {
		t.Skipf("SKIP: this host cannot create a directory under /var/tmp (%v); the subject is a worktree there", err)
	}
	// Cleanup owns exactly what MkdirTemp minted, nothing wider.
	t.Cleanup(func() {
		if strings.HasPrefix(parent, "/var/tmp/mgit-e2e-wtpath-") {
			_ = os.RemoveAll(parent)
		}
	})

	sb := launchRealVMForSyncIn(t, "wtpath", "MGIT-230.7", parent)
	if got := guestRead(t, sb, filepath.Join(sb.worktree, "seed.txt")); got != "host work" {
		t.Fatalf("the guest must read the worktree at its identical path %s; got %q", sb.worktree, got)
	}
	t.Logf("REAL VM PASS: a worktree under /var/tmp (a directory the guest image ships /var for) is mounted at %s", sb.worktree)
}
