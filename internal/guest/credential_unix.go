//go:build unix

package guest

import (
	"os"
	"os/exec"
	"syscall"

	"github.com/hyper-swe/mgit/internal/model"
)

// startAs arranges for cmd to start as id: nothing when the supervisor
// already is that identity, a credential otherwise. A switch this process
// cannot make fails at start (EPERM) — the child never runs as the
// supervisor instead. Refs: MGIT-151
func startAs(cmd *exec.Cmd, id model.GuestIdentity) error {
	if id.UID == os.Getuid() && id.GID == os.Getgid() {
		return nil
	}
	uid, gid := uint32(id.UID), uint32(id.GID) //nolint:gosec // G115: ids are validated non-negative by the model
	cmd.SysProcAttr = &syscall.SysProcAttr{Credential: &syscall.Credential{Uid: uid, Gid: gid, NoSetGroups: true}}
	return nil
}
