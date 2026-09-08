//go:build unix

package guest

import (
	"os"
	"syscall"
)

// fileOwner returns the uid and gid of a stat result. Refs: MGIT-151
func fileOwner(fi os.FileInfo) (uid, gid int, ok bool) {
	st, isStat := fi.Sys().(*syscall.Stat_t)
	if !isStat {
		return 0, 0, false
	}
	return int(st.Uid), int(st.Gid), true
}
