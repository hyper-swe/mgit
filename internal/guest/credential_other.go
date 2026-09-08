//go:build !unix

package guest

import (
	"fmt"
	"os"
	"os/exec"

	"github.com/hyper-swe/mgit/internal/model"
)

// startAs cannot switch identity on this platform; the guest never runs
// here, and the package must still build where the CLI is built. Asked for
// another identity, it refuses rather than runs the command as itself.
func startAs(_ *exec.Cmd, id model.GuestIdentity) error {
	if id.UID == os.Getuid() && id.GID == os.Getgid() {
		return nil
	}
	return fmt.Errorf("cannot run as uid %d gid %d: identity switching is not supported on this platform", id.UID, id.GID)
}
