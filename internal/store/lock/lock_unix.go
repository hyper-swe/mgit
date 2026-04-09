//go:build !windows

package lock

import (
	"os"
	"syscall"
)

// osNoFollow is the platform flag that prevents opening symlinks.
const osNoFollow = syscall.O_NOFOLLOW

// lockFD acquires an exclusive non-blocking advisory lock on the file
// using flock(2). The lock is automatically released when the file
// descriptor is closed (either explicitly or via process exit).
func lockFD(f *os.File) error {
	return syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
}

// unlockFD releases the advisory lock on the file.
func unlockFD(f *os.File) error {
	return syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
}
