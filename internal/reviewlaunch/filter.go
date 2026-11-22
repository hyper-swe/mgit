// Package reviewlaunch is the guest-side launcher of the reviewer-ro profile
// (FEAT-3.347.1 R1). It runs exactly one pinned entrypoint with no way to
// start another program: the entrypoint's bytes are hashed through the very
// descriptor that is executed, privileges are shed, and a seccomp filter
// leaves one exec (by that descriptor) and no fork. The filter builder, the
// one-exec decision and the refusal vocabulary are pure and portable; the
// launcher itself is Linux only.
package reviewlaunch

import (
	"fmt"
	"runtime"
)

// Arch names a guest architecture the filter has a syscall table for.
type Arch int

// The supported guest architectures.
const (
	ArchAmd64 Arch = iota + 1
	ArchArm64
)

func (a Arch) String() string {
	switch a {
	case ArchAmd64:
		return "amd64"
	case ArchArm64:
		return "arm64"
	}
	return fmt.Sprintf("arch(%d)", int(a))
}

// HostArch is the architecture this binary was built for; amd64 stands in
// for anything else so portable tests still run.
func HostArch() Arch {
	if runtime.GOARCH == "arm64" {
		return ArchArm64
	}
	return ArchAmd64
}

func hostArch() Arch { return HostArch() }

// AuditArch is the AUDIT_ARCH value the kernel reports for a.
func AuditArch(a Arch) uint32 {
	if a == ArchArm64 {
		return 0xc00000b7
	}
	return 0xc000003e
}

// Seccomp return values and classic BPF opcodes the filter uses.
const (
	RetKillProcess uint32 = 0x80000000
	RetUserNotif   uint32 = 0x7fc00000
	RetErrno       uint32 = 0x00050000
	RetAllow       uint32 = 0x7fff0000

	opLdAbs uint16 = 0x20
	opJeq   uint16 = 0x15
	opJset  uint16 = 0x45
	opRet   uint16 = 0x06

	// CloneThread is CLONE_THREAD: a clone carrying it makes a thread of
	// this process, not a new process.
	CloneThread uint64 = 0x00010000
	// AtEmptyPath is AT_EMPTY_PATH: execveat on the descriptor itself.
	AtEmptyPath uint64 = 0x1000

	errnoPerm  uint32 = 1
	errnoNosys uint32 = 38
)

// Insn is one classic BPF instruction (struct sock_filter).
type Insn struct {
	Code uint16
	Jt   uint8
	Jf   uint8
	K    uint32
}

// SyscallNumbers are the numbers the filter reasons about on one
// architecture. Fork and Vfork are zero where the architecture has none.
type SyscallNumbers struct {
	Read, Write                uint32
	Execve, Execveat           uint32
	Clone, Clone3, Fork, Vfork uint32
}

// Syscalls returns the table for a.
func Syscalls(a Arch) SyscallNumbers {
	if a == ArchArm64 {
		return SyscallNumbers{Read: 63, Write: 64, Execve: 221, Execveat: 281, Clone: 220, Clone3: 435}
	}
	return SyscallNumbers{Read: 0, Write: 1, Execve: 59, Execveat: 322, Clone: 56, Clone3: 435, Fork: 57, Vfork: 58}
}

func ins(code uint16, jt, jf uint8, k uint32) Insn { return Insn{code, jt, jf, k} }

// BuildFilter returns the seccomp program of the profile: a foreign ABI is
// killed; execve by path is refused; execveat goes to the supervisor, which
// allows exactly one; fork and vfork are refused; clone is allowed only as a
// thread; clone3 answers ENOSYS so C libraries fall back to clone. Everything
// else is allowed: the profile's other limits (read-only image, no network,
// no credentials) are the guest's, not the filter's.
func BuildFilter(a Arch) ([]Insn, error) {
	if a != ArchAmd64 && a != ArchArm64 {
		return nil, fmt.Errorf("reviewlaunch: no syscall table for %v", a)
	}
	n := Syscalls(a)
	f := []Insn{
		ins(opLdAbs, 0, 0, 4), // seccomp_data.arch
		ins(opJeq, 1, 0, AuditArch(a)),
		ins(opRet, 0, 0, RetKillProcess),
		ins(opLdAbs, 0, 0, 0), // seccomp_data.nr
	}
	if a == ArchAmd64 {
		// x32 syscalls report the same audit arch with bit 30 set in the
		// number and would slip past every exact-number rule below.
		f = append(f, ins(opJset, 0, 1, 0x40000000), ins(opRet, 0, 0, RetKillProcess))
	}
	verdict := func(nr, v uint32) {
		f = append(f, ins(opJeq, 0, 1, nr), ins(opRet, 0, 0, v))
	}
	verdict(n.Execve, RetErrno|errnoPerm)
	verdict(n.Execveat, RetUserNotif)
	verdict(n.Clone3, RetErrno|errnoNosys)
	if a == ArchAmd64 {
		verdict(n.Fork, RetErrno|errnoPerm)
		verdict(n.Vfork, RetErrno|errnoPerm)
	}
	f = append(f,
		ins(opJeq, 0, 4, n.Clone),
		ins(opLdAbs, 0, 0, 16), // args[0] low word: the clone flags
		ins(opJset, 1, 0, uint32(CloneThread)),
		ins(opRet, 0, 0, RetErrno|errnoPerm),
		ins(opRet, 0, 0, RetAllow),
		ins(opLdAbs, 0, 0, 0),
		ins(opRet, 0, 0, RetAllow),
	)
	return f, nil
}

// Decision is the supervisor's answer to one notification.
type Decision struct {
	Allow bool
	Errno int
}

// ExecDecider allows exactly one execveat, the one that runs the verified
// descriptor with AT_EMPTY_PATH, and refuses everything else.
type ExecDecider struct {
	fd       uint64
	execveat uint32
	used     bool
}

// NewExecDecider returns a decider for the entrypoint descriptor fd.
func NewExecDecider(fd int, a Arch) *ExecDecider {
	return &ExecDecider{fd: uint64(fd), execveat: Syscalls(a).Execveat}
}

// Decide answers a notification for syscall nr with raw argument registers.
// pathEmpty says the path argument points at an empty string; with a
// non-empty path execveat ignores the descriptor and runs something else.
// Only a matching call uses up the one exec.
func (d *ExecDecider) Decide(nr uint32, args [6]uint64, pathEmpty bool) Decision {
	deny := Decision{Errno: int(errnoPerm)}
	if nr != d.execveat || d.used || !pathEmpty {
		return deny
	}
	if uint64(uint32(args[0])) != d.fd || args[4]&AtEmptyPath == 0 {
		return deny
	}
	d.used = true
	return Decision{Allow: true}
}

// RefusalError codes, named by the reviewer-ro/v1 contract (item 4).
const (
	CodeHashMismatch        = "HASH_MISMATCH"
	CodeBadDigest           = "BAD_DIGEST"
	CodeNotELF              = "NOT_ELF"
	CodePolicyUnenforceable = "POLICY_UNENFORCEABLE"
	CodePrivileged          = "PRIVILEGED"
	CodeLaunchFailed        = "LAUNCH_FAILED"
	CodeProfileUnavailable  = "PROFILE_UNAVAILABLE"
)

// RefusalError is a launch the launcher declined or could not carry out. The
// reviewer was not started.
type RefusalError struct {
	Code   string `json:"code"`
	Detail string `json:"detail"`
}

func (r *RefusalError) Error() string { return "reviewlaunch: " + r.Code + ": " + r.Detail }

func refuse(code, format string, a ...any) error {
	return &RefusalError{Code: code, Detail: fmt.Sprintf(format, a...)}
}
