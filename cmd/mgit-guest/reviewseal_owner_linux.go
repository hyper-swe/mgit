//go:build linux

package main

import (
	"fmt"
	"os"
	"syscall"
)

// ownedByRootWithGroup reports an error unless path is owned by uid 0 with the
// given group, which is what a seal that held looks like from the outside.
func ownedByRootWithGroup(path string, gid int) error {
	fi, err := os.Lstat(path)
	if err != nil {
		return err
	}
	st, ok := fi.Sys().(*syscall.Stat_t)
	if !ok || st.Uid != 0 || int(st.Gid) != gid {
		return fmt.Errorf("%s is not owned by root with group %d", path, gid)
	}
	return nil
}
