//go:build linux && amd64

package reviewlaunch

import (
	"testing"

	"golang.org/x/sys/unix"
)

// The table is typed by hand; the platform's own constants say whether it is right.
func TestWriteDeniedNumbers_MatchThePlatformTable(t *testing.T) {
	want := map[string]uint32{
		"open": unix.SYS_OPEN, "creat": unix.SYS_CREAT, "link": unix.SYS_LINK, "unlink": unix.SYS_UNLINK,
		"symlink": unix.SYS_SYMLINK, "rename": unix.SYS_RENAME, "mkdir": unix.SYS_MKDIR, "rmdir": unix.SYS_RMDIR,
		"chmod": unix.SYS_CHMOD, "fchmod": unix.SYS_FCHMOD, "chown": unix.SYS_CHOWN, "fchown": unix.SYS_FCHOWN,
		"lchown": unix.SYS_LCHOWN, "truncate": unix.SYS_TRUNCATE, "ftruncate": unix.SYS_FTRUNCATE,
		"mknod": unix.SYS_MKNOD, "utime": unix.SYS_UTIME, "utimes": unix.SYS_UTIMES, "mount": unix.SYS_MOUNT,
		"umount2": unix.SYS_UMOUNT2, "pivot_root": unix.SYS_PIVOT_ROOT, "chroot": unix.SYS_CHROOT,
		"setxattr": unix.SYS_SETXATTR, "lsetxattr": unix.SYS_LSETXATTR, "fsetxattr": unix.SYS_FSETXATTR,
		"removexattr": unix.SYS_REMOVEXATTR, "lremovexattr": unix.SYS_LREMOVEXATTR, "fremovexattr": unix.SYS_FREMOVEXATTR,
		"openat": unix.SYS_OPENAT, "mkdirat": unix.SYS_MKDIRAT, "mknodat": unix.SYS_MKNODAT, "fchownat": unix.SYS_FCHOWNAT,
		"futimesat": unix.SYS_FUTIMESAT, "unlinkat": unix.SYS_UNLINKAT, "renameat": unix.SYS_RENAMEAT,
		"linkat": unix.SYS_LINKAT, "symlinkat": unix.SYS_SYMLINKAT, "fchmodat": unix.SYS_FCHMODAT,
		"utimensat": unix.SYS_UTIMENSAT, "fallocate": unix.SYS_FALLOCATE, "renameat2": unix.SYS_RENAMEAT2,
		"memfd_create": unix.SYS_MEMFD_CREATE, "open_by_handle_at": unix.SYS_OPEN_BY_HANDLE_AT, "bind": unix.SYS_BIND,
		"io_uring_setup": unix.SYS_IO_URING_SETUP, "io_uring_enter": unix.SYS_IO_URING_ENTER,
		"io_uring_register": unix.SYS_IO_URING_REGISTER, "open_tree": unix.SYS_OPEN_TREE, "move_mount": unix.SYS_MOVE_MOUNT,
		"fsopen": unix.SYS_FSOPEN, "fsconfig": unix.SYS_FSCONFIG, "fsmount": unix.SYS_FSMOUNT, "fspick": unix.SYS_FSPICK,
		"openat2": unix.SYS_OPENAT2, "mount_setattr": unix.SYS_MOUNT_SETATTR, "fchmodat2": unix.SYS_FCHMODAT2,
	}
	checkNumbers(t, ArchAmd64, want)
}
