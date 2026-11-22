package reviewlaunch

import "testing"

const (
	oRdonly  = 0
	oWronly  = 1
	oRdwr    = 2
	oCreat   = 0x40
	oTrunc   = 0x200
	oAppend  = 0x400
	oTmpfile = 0x410000
	oCloexec = 0x80000
	oDir     = 0x10000
)

// The profile promises no filesystem writes. A same-uid program that can
// create or change any file its uid can reach has no such limit, so the
// filter refuses every call that creates, changes or removes a file, a
// directory, a link, an attribute or a mount, and refuses a descriptor opened
// for writing.
func TestFilter_RefusesEveryFileMutation(t *testing.T) {
	for _, arch := range []Arch{ArchAmd64, ArchArm64} {
		prog, err := BuildFilter(arch)
		if err != nil {
			t.Fatal(err)
		}
		deny := WriteDeniedSyscalls(arch)
		if len(deny) < 25 {
			t.Fatalf("%v: only %d denied syscalls; the table was cut down", arch, len(deny))
		}
		for _, d := range deny {
			want := errnoVerdict(1)
			if d.Enosys {
				want = errnoVerdict(38)
			}
			if got := evalBPF(t, prog, AuditArch(arch), d.Nr, [6]uint64{}); got != want {
				t.Errorf("%v %s: verdict %#x, want %#x", arch, d.Name, got, want)
			}
		}
	}
}

func TestFilter_OpenIsReadOnly(t *testing.T) {
	for _, arch := range []Arch{ArchAmd64, ArchArm64} {
		prog, _ := BuildFilter(arch)
		n := Syscalls(arch)
		type call struct {
			nr   uint32
			args [6]uint64
		}
		mk := func(flags uint64) []call {
			c := []call{{n.Openat, [6]uint64{0, 0, flags}}}
			if n.Open != 0 {
				c = append(c, call{n.Open, [6]uint64{0, flags}})
			}
			return c
		}
		for name, flags := range map[string]uint64{
			"read only": oRdonly, "read only, cloexec": oRdonly | oCloexec, "directory": oRdonly | oDir,
		} {
			for _, c := range mk(flags) {
				if got := evalBPF(t, prog, AuditArch(arch), c.nr, c.args); got != RetAllow {
					t.Errorf("%v %s nr %d: verdict %#x, want allow", arch, name, c.nr, got)
				}
			}
		}
		for name, flags := range map[string]uint64{
			"write only": oWronly, "read write": oRdwr, "create": oRdonly | oCreat, "truncate": oRdonly | oTrunc,
			"append": oRdonly | oAppend, "tmpfile": oRdonly | oTmpfile, "create+write": oWronly | oCreat | oTrunc,
			"high bits set too": (1 << 40) | oWronly,
		} {
			for _, c := range mk(flags) {
				if got := evalBPF(t, prog, AuditArch(arch), c.nr, c.args); got != errnoVerdict(1) {
					t.Errorf("%v %s nr %d: verdict %#x, want EPERM", arch, name, c.nr, got)
				}
			}
		}
	}
}

// The new rules must not disturb the old ones, and ordinary I/O stays.
func TestFilter_OrdinaryIOIsStillAllowed(t *testing.T) {
	for _, arch := range []Arch{ArchAmd64, ArchArm64} {
		prog, _ := BuildFilter(arch)
		n := Syscalls(arch)
		for name, nr := range map[string]uint32{"read": n.Read, "write": n.Write} {
			if got := evalBPF(t, prog, AuditArch(arch), nr, [6]uint64{}); got != RetAllow {
				t.Errorf("%v %s: %#x", arch, name, got)
			}
		}
		if got := evalBPF(t, prog, AuditArch(arch), n.Clone, [6]uint64{CloneThread}); got != RetAllow {
			t.Errorf("%v thread clone broke: %#x", arch, got)
		}
		if got := evalBPF(t, prog, AuditArch(arch), n.Execveat, [6]uint64{}); got != RetUserNotif {
			t.Errorf("%v execveat broke: %#x", arch, got)
		}
	}
}

// openat2 takes its flags in a struct the filter cannot read, so it answers
// "not implemented" (a library then falls back to openat, which is judged);
// the number is the literal the kernel uses on both architectures.
func TestFilter_Openat2IsNotImplemented(t *testing.T) {
	for _, arch := range []Arch{ArchAmd64, ArchArm64} {
		prog, _ := BuildFilter(arch)
		if got := evalBPF(t, prog, AuditArch(arch), 437, [6]uint64{}); got != errnoVerdict(38) {
			t.Errorf("%v openat2: verdict %#x, want ENOSYS", arch, got)
		}
	}
}

// A syscall the table does not know yet (the kernel adds them every release)
// must not slip through as allowed: above the highest known number every call
// answers "not implemented", and a library falls back to a call that is judged.
func TestFilter_FailsClosedAboveTheTable(t *testing.T) {
	for _, arch := range []Arch{ArchAmd64, ArchArm64} {
		prog, _ := BuildFilter(arch)
		for _, nr := range []uint32{453, 463, 466, 467, 469, 470, 1000, 0x3fffffff} {
			if got := evalBPF(t, prog, AuditArch(arch), nr, [6]uint64{}); got != errnoVerdict(38) {
				t.Errorf("%v nr %d: verdict %#x, want ENOSYS", arch, nr, got)
			}
		}
		// The numbers just below the line keep their own verdicts.
		if got := evalBPF(t, prog, AuditArch(arch), 452, [6]uint64{}); got != errnoVerdict(1) {
			t.Errorf("%v fchmodat2: verdict %#x, want EPERM", arch, got)
		}
		if got := evalBPF(t, prog, AuditArch(arch), Syscalls(arch).Read, [6]uint64{}); got != RetAllow {
			t.Errorf("%v read: verdict %#x, want allow", arch, got)
		}
	}
}

// ioctl is an allowlist of requests: terminal and descriptor housekeeping. The
// requests that change an inode's flags, extended attributes or contents
// (FS_IOC_SETFLAGS, FS_IOC_FSSETXATTR, FICLONE, FITRIM and the rest) are not on it.
func TestFilter_IoctlIsAnAllowlist(t *testing.T) {
	allowed := []uint64{0x5401, 0x5402, 0x5403, 0x5404, 0x5413, 0x541b, 0x5421, 0x5450, 0x5451}
	denied := []uint64{0x40086602, 0x401c581f, 0x40049409, 0xc0185879, 0x1234, 0x5400, 0x5405, 0x8933}
	for _, arch := range []Arch{ArchAmd64, ArchArm64} {
		prog, _ := BuildFilter(arch)
		nr := Syscalls(arch).Ioctl
		if nr == 0 {
			t.Fatalf("%v: no ioctl number", arch)
		}
		for _, c := range allowed {
			if got := evalBPF(t, prog, AuditArch(arch), nr, [6]uint64{3, c}); got != RetAllow {
				t.Errorf("%v ioctl %#x: verdict %#x, want allow", arch, c, got)
			}
		}
		for _, c := range denied {
			if got := evalBPF(t, prog, AuditArch(arch), nr, [6]uint64{3, c}); got != errnoVerdict(1) {
				t.Errorf("%v ioctl %#x: verdict %#x, want EPERM", arch, c, got)
			}
		}
		// The request is a 32-bit value: high bits in the register do not change it.
		if got := evalBPF(t, prog, AuditArch(arch), nr, [6]uint64{3, 1<<40 | 0x40086602}); got != errnoVerdict(1) {
			t.Errorf("%v ioctl with high bits set: verdict %#x, want EPERM", arch, got)
		}
	}
}

// The calls named for the review: message queues, the System V family, and
// the descriptor-taking pidfd calls.
func TestFilter_NamesTheIPCAndPidfdCalls(t *testing.T) {
	want := []string{"mq_open", "mq_unlink", "mq_timedsend", "shmget", "shmat", "msgget", "msgsnd", "semget", "semop",
		"pidfd_open", "pidfd_getfd", "setxattrat", "removexattrat", "open_tree_attr", "file_setattr"}
	for _, arch := range []Arch{ArchAmd64, ArchArm64} {
		have := map[string]bool{}
		for _, d := range WriteDeniedSyscalls(arch) {
			have[d.Name] = true
		}
		for _, n := range want {
			if !have[n] {
				t.Errorf("%v: %s is not in the table", arch, n)
			}
		}
	}
}
