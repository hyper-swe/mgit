package reviewlaunch

// OpenWriteMask is every open flag that makes an open a write or a create:
// O_WRONLY, O_RDWR, O_CREAT, O_TRUNC, O_APPEND and the O_TMPFILE bit.
const OpenWriteMask uint32 = 0x1 | 0x2 | 0x40 | 0x200 | 0x400 | 0x400000

// NamedSyscall is one syscall the profile refuses outright. Enosys marks the
// ones answered "not implemented", so a library falls back to the plain call
// the filter already judges.
type NamedSyscall struct {
	Name   string
	Nr     uint32
	Enosys bool
}

// WriteDeniedSyscalls lists every call that creates, changes or removes a
// file, directory, link, attribute or mount, or that gives a way around the
// open-flags rule (a rewritten open, an io_uring submission, a mount API). The
// open and openat calls themselves are judged by their flags, not listed.
func WriteDeniedSyscalls(a Arch) []NamedSyscall {
	var out []NamedSyscall
	add := func(name string, nr uint32) { out = append(out, NamedSyscall{Name: name, Nr: nr}) }
	// Numbers shared by both architectures (the unified syscall table).
	both := func() {
		add("io_uring_setup", 425)
		add("io_uring_enter", 426)
		add("io_uring_register", 427)
		add("open_tree", 428)
		add("move_mount", 429)
		add("fsopen", 430)
		add("fsconfig", 431)
		add("fsmount", 432)
		add("fspick", 433)
		out = append(out, NamedSyscall{Name: "openat2", Nr: 437, Enosys: true})
		add("mount_setattr", 442)
		add("fchmodat2", 452)
	}
	if a == ArchArm64 {
		for name, nr := range map[string]uint32{
			"fchmod": 52, "fchown": 55, "truncate": 45, "ftruncate": 46, "mount": 40, "umount2": 39,
			"pivot_root": 41, "chroot": 51, "setxattr": 5, "lsetxattr": 6, "fsetxattr": 7,
			"removexattr": 14, "lremovexattr": 15, "fremovexattr": 16, "mkdirat": 34, "mknodat": 33,
			"fchownat": 54, "unlinkat": 35, "renameat": 38, "linkat": 37, "symlinkat": 36, "fchmodat": 53,
			"utimensat": 88, "fallocate": 47, "renameat2": 276, "memfd_create": 279,
			"open_by_handle_at": 265, "bind": 200,
		} {
			add(name, nr)
		}
		both()
		return out
	}
	for name, nr := range map[string]uint32{
		"creat": 85, "link": 86, "unlink": 87, "symlink": 88, "rename": 82, "mkdir": 83, "rmdir": 84,
		"chmod": 90, "fchmod": 91, "chown": 92, "fchown": 93, "lchown": 94, "truncate": 76, "ftruncate": 77,
		"mknod": 133, "utime": 132, "utimes": 235, "mount": 165, "umount2": 166, "pivot_root": 155,
		"chroot": 161, "setxattr": 188, "lsetxattr": 189, "fsetxattr": 190, "removexattr": 197,
		"lremovexattr": 198, "fremovexattr": 199, "mkdirat": 258, "mknodat": 259, "fchownat": 260,
		"futimesat": 261, "unlinkat": 263, "renameat": 264, "linkat": 265, "symlinkat": 266,
		"fchmodat": 268, "utimensat": 280, "fallocate": 285, "renameat2": 316, "memfd_create": 319,
		"open_by_handle_at": 304, "bind": 49,
	} {
		add(name, nr)
	}
	both()
	return out
}
