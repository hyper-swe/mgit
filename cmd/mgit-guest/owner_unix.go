//go:build unix

package main

import (
	"os"
	"syscall"
)

// ownerOf returns the uid and gid a FileInfo from os.Stat/os.Lstat carries.
// It asserts *syscall.Stat_t, the type os actually puts in Sys(); a
// golang.org/x/sys/unix.Stat_t is a DIFFERENT type, and asserting it never
// holds, which is how the /etc shadow came to chown nothing at all. ok is
// false when the FileInfo carries no platform stat, so an unknown owner is
// never mistaken for root. Refs: MGIT-230.3, MGIT-89
func ownerOf(info os.FileInfo) (uid, gid int, ok bool) {
	st, isStat := info.Sys().(*syscall.Stat_t)
	if !isStat || st == nil {
		return 0, 0, false
	}
	return int(st.Uid), int(st.Gid), true
}
