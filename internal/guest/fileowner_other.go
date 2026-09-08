//go:build !unix

package guest

import "os"

// fileOwner has no owner to read on this platform; the guest never runs
// here, and the package must still build where the CLI is built.
func fileOwner(os.FileInfo) (uid, gid int, ok bool) { return 0, 0, false }
