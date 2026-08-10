//go:build unix

package artifactexport

import (
	"io/fs"
	"syscall"
)

// noFollowFlag makes the export open a planned source file WITHOUT following a
// symlink, closing the plan-to-copy window in which a hostile guest could
// replace a regular file with a link to a host path outside the subtree.
const noFollowFlag = syscall.O_NOFOLLOW

// linkIdentity reports a file's inode and link count so hardlink containment
// can be decided. ok is false when the platform does not expose them, which
// the caller treats as a refusal (fail closed).
func linkIdentity(info fs.FileInfo) (id, links uint64, ok bool) {
	st, isStat := info.Sys().(*syscall.Stat_t)
	if !isStat {
		return 0, 0, false
	}
	return st.Ino, uint64(st.Nlink), true
}
