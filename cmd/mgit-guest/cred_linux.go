//go:build linux

package main

import (
	"fmt"
	"syscall"
)

const tmpfsMagic = 0x01021994

// ensureTmpfs makes dir memory-backed: it is left alone when it already is,
// and a fresh tmpfs is mounted on it otherwise. The guest's root is an image
// on a disk, so a credential must never be written to it.
func ensureTmpfs(dir string) error {
	var st syscall.Statfs_t
	if err := syscall.Statfs(dir, &st); err != nil {
		return err
	}
	if st.Type == tmpfsMagic {
		return nil
	}
	if err := syscall.Mount("tmpfs", dir, "tmpfs", syscall.MS_NOSUID|syscall.MS_NODEV|syscall.MS_NOEXEC, "mode=0750,size=64k"); err != nil {
		return err
	}
	if err := syscall.Statfs(dir, &st); err != nil || st.Type != tmpfsMagic {
		return fmt.Errorf("not a tmpfs after the mount")
	}
	return nil
}
