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
