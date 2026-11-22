package reviewlaunch

import "testing"

// evalBPF runs a classic BPF program over a seccomp_data record. Only the
// instructions the filter builder emits are supported; anything else fails
// the test, so a new opcode cannot slip in unexamined.
func evalBPF(t *testing.T, prog []Insn, arch uint32, nr uint32, args [6]uint64) uint32 {
	t.Helper()
	var acc uint32
	load := func(off uint32) uint32 {
		switch {
		case off == 0:
			return nr
		case off == 4:
			return arch
		case off >= 16 && off < 64 && off%4 == 0:
			a := args[(off-16)/8]
			if (off-16)%8 == 0 {
				return uint32(a)
			}
			return uint32(a >> 32)
		}
		t.Fatalf("filter loads unsupported offset %d", off)
		return 0
	}
	for pc := 0; pc < len(prog); pc++ {
		in := prog[pc]
		switch in.Code {
		case opLdAbs:
			acc = load(in.K)
		case opJeq:
			if acc == in.K {
				pc += int(in.Jt)
			} else {
				pc += int(in.Jf)
			}
		case opJset:
			if acc&in.K != 0 {
				pc += int(in.Jt)
			} else {
				pc += int(in.Jf)
			}
		case opRet:
			return in.K
		default:
			t.Fatalf("filter uses unsupported opcode %#x at %d", in.Code, pc)
		}
	}
	t.Fatalf("filter fell off the end without a verdict")
	return 0
}

func errnoVerdict(e uint32) uint32 { return RetErrno | e }

func TestFilter_PerSyscallVerdicts(t *testing.T) {
	const (
		eperm  = 1
		enosys = 38
	)
	type row struct {
		name string
		nr   uint32
		args [6]uint64
		want uint32
	}
	for _, arch := range []Arch{ArchAmd64, ArchArm64} {
		prog, err := BuildFilter(arch)
		if err != nil {
			t.Fatalf("BuildFilter(%v): %v", arch, err)
		}
		n := Syscalls(arch)
		rows := []row{
			{"execve path is refused", n.Execve, [6]uint64{}, errnoVerdict(eperm)},
			{"execveat goes to the supervisor", n.Execveat, [6]uint64{}, RetUserNotif},
			{"clone without CLONE_THREAD is refused", n.Clone, [6]uint64{0x11}, errnoVerdict(eperm)},
			{"clone with CLONE_THREAD is allowed", n.Clone, [6]uint64{CloneThread | 0x100}, RetAllow},
			{"clone3 is ENOSYS so libc falls back to clone", n.Clone3, [6]uint64{}, errnoVerdict(enosys)},
			{"ptrace is refused", n.Ptrace, [6]uint64{}, errnoVerdict(eperm)},
			{"process_vm_writev is refused", n.ProcessVMWritev, [6]uint64{}, errnoVerdict(eperm)},
			{"read is allowed", n.Read, [6]uint64{}, RetAllow},
			{"write is allowed", n.Write, [6]uint64{}, RetAllow},
		}
		if arch == ArchAmd64 {
			rows = append(rows,
				row{"fork is refused", n.Fork, [6]uint64{}, errnoVerdict(eperm)},
				row{"vfork is refused", n.Vfork, [6]uint64{}, errnoVerdict(eperm)})
		}
		for _, r := range rows {
			if got := evalBPF(t, prog, AuditArch(arch), r.nr, r.args); got != r.want {
				t.Errorf("%v %s: verdict %#x, want %#x", arch, r.name, got, r.want)
			}
		}
		if arch == ArchAmd64 {
			// x32 syscalls carry the same audit arch with bit 30 set in the
			// number; none may reach the allow rule.
			for _, nr := range []uint32{0x40000000 | n.Execve, 0x40000000 | 520, 0x40000000 | 545, 0x40000000} {
				if got := evalBPF(t, prog, AuditArch(arch), nr, [6]uint64{}); got != RetKillProcess {
					t.Errorf("amd64 x32 nr %#x: verdict %#x, want kill", nr, got)
				}
			}
		}
		// A foreign ABI (the 32-bit compat table on amd64) must not slip
		// past number-based rules.
		if got := evalBPF(t, prog, 0x40000003, n.Read, [6]uint64{}); got != RetKillProcess {
			t.Errorf("%v wrong audit arch: verdict %#x, want kill %#x", arch, got, RetKillProcess)
		}
	}
}

func TestBuildFilter_UnknownArchIsRefused(t *testing.T) {
	if _, err := BuildFilter(Arch(99)); err == nil {
		t.Fatal("BuildFilter accepted an unknown architecture")
	}
}
