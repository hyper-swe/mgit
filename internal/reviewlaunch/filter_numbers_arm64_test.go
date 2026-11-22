//go:build linux && arm64

package reviewlaunch

import (
	"testing"

	"golang.org/x/sys/unix"
)

func TestWriteDeniedNumbers_MatchThePlatformTable(t *testing.T) {
	want := map[string]uint32{
		"fchmod": unix.SYS_FCHMOD, "fchown": unix.SYS_FCHOWN, "truncate": unix.SYS_TRUNCATE, "ftruncate": unix.SYS_FTRUNCATE,
		"mount": unix.SYS_MOUNT, "umount2": unix.SYS_UMOUNT2, "pivot_root": unix.SYS_PIVOT_ROOT, "chroot": unix.SYS_CHROOT,
		"setxattr": unix.SYS_SETXATTR, "lsetxattr": unix.SYS_LSETXATTR, "fsetxattr": unix.SYS_FSETXATTR,
		"removexattr": unix.SYS_REMOVEXATTR, "lremovexattr": unix.SYS_LREMOVEXATTR, "fremovexattr": unix.SYS_FREMOVEXATTR,
		"openat": unix.SYS_OPENAT, "mkdirat": unix.SYS_MKDIRAT, "mknodat": unix.SYS_MKNODAT, "fchownat": unix.SYS_FCHOWNAT,
		"unlinkat": unix.SYS_UNLINKAT, "renameat": unix.SYS_RENAMEAT, "linkat": unix.SYS_LINKAT, "symlinkat": unix.SYS_SYMLINKAT,
		"fchmodat": unix.SYS_FCHMODAT, "utimensat": unix.SYS_UTIMENSAT, "fallocate": unix.SYS_FALLOCATE,
		"renameat2": unix.SYS_RENAMEAT2, "memfd_create": unix.SYS_MEMFD_CREATE, "open_by_handle_at": unix.SYS_OPEN_BY_HANDLE_AT,
		"bind": unix.SYS_BIND, "io_uring_setup": unix.SYS_IO_URING_SETUP, "io_uring_enter": unix.SYS_IO_URING_ENTER,
		"io_uring_register": unix.SYS_IO_URING_REGISTER, "open_tree": unix.SYS_OPEN_TREE, "move_mount": unix.SYS_MOVE_MOUNT,
		"fsopen": unix.SYS_FSOPEN, "fsconfig": unix.SYS_FSCONFIG, "fsmount": unix.SYS_FSMOUNT, "fspick": unix.SYS_FSPICK,
		"openat2": unix.SYS_OPENAT2, "mount_setattr": unix.SYS_MOUNT_SETATTR, "fchmodat2": unix.SYS_FCHMODAT2,
		"mq_open": unix.SYS_MQ_OPEN, "mq_unlink": unix.SYS_MQ_UNLINK, "mq_timedsend": unix.SYS_MQ_TIMEDSEND,
		"shmget": unix.SYS_SHMGET, "shmat": unix.SYS_SHMAT, "msgget": unix.SYS_MSGGET, "msgsnd": unix.SYS_MSGSND,
		"semget": unix.SYS_SEMGET, "semop": unix.SYS_SEMOP, "pidfd_open": unix.SYS_PIDFD_OPEN, "pidfd_getfd": unix.SYS_PIDFD_GETFD,
		"setxattrat": unix.SYS_SETXATTRAT, "removexattrat": unix.SYS_REMOVEXATTRAT, "open_tree_attr": unix.SYS_OPEN_TREE_ATTR,
		"file_setattr": unix.SYS_FILE_SETATTR,
	}
	checkNumbers(t, ArchArm64, want)
}

const unixIoctl = unix.SYS_IOCTL
