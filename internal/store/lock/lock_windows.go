//go:build windows

package lock

import (
	"os"

	"golang.org/x/sys/windows"
)

// osNoFollow is a no-op on Windows. Symlink protection is handled
// via the Lstat check in Acquire (Windows symlinks are uncommon
// and require elevated privileges to create).
const osNoFollow = 0

// lockFD acquires an exclusive non-blocking lock on the file using
// LockFileEx with LOCKFILE_EXCLUSIVE_LOCK | LOCKFILE_FAIL_IMMEDIATELY.
// The lock is automatically released when the file handle is closed
// or the process exits.
func lockFD(f *os.File) error {
	handle := windows.Handle(f.Fd())
	var ol windows.Overlapped
	return windows.LockFileEx(
		handle,
		windows.LOCKFILE_EXCLUSIVE_LOCK|windows.LOCKFILE_FAIL_IMMEDIATELY,
		0,
		1, 0,
		&ol,
	)
}

// unlockFD releases the file lock.
func unlockFD(f *os.File) error {
	handle := windows.Handle(f.Fd())
	var ol windows.Overlapped
	return windows.UnlockFileEx(handle, 0, 1, 0, &ol)
}
