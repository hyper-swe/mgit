//go:build unix

package main

import (
	"os"
	"syscall"
)

// ownedByUser reports whether fi is owned by the current user. It guards the
// shared fallback runtime dir against a squatter. Refs: MGIT-279
func ownedByUser(fi os.FileInfo) bool {
	st, ok := fi.Sys().(*syscall.Stat_t)
	return ok && int(st.Uid) == os.Getuid()
}
