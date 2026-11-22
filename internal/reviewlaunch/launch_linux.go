//go:build linux

package reviewlaunch

import (
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"io"
	"os"
	"os/exec"
	"runtime"
	"strconv"
	"strings"
	"syscall"
	"unsafe"
)

const stageArg = "__review_stage__"

// Config describes one review launch.
type Config struct {
	// Entrypoint is the absolute path of the pinned executable and SHA256 its
	// lower-case hex digest; Argv are its arguments after argv[0].
	Entrypoint string
	SHA256     string
	Argv       []string
	// Dir is the working directory; empty keeps the current one.
	Dir string
	// DropTo is the unprivileged identity to run as. A root launcher without
	// one is refused.
	DropTo *Identity
	Stdin  io.Reader
	Stdout io.Writer
	Stderr io.Writer
}

// Identity is the unprivileged user and group the entrypoint runs as.
type Identity struct{ UID, GID int }

// stageConfig is what the parent hands the stage on its command line; it
// holds nothing secret.
type stageConfig struct {
	Entrypoint string    `json:"entrypoint"`
	SHA256     string    `json:"sha256"`
	Argv       []string  `json:"argv"`
	Dir        string    `json:"dir"`
	DropTo     *Identity `json:"drop_to"`
}

const (
	sysSeccompAmd64 = 317
	sysSeccompArm64 = 277

	prSetNoNewPrivs = 38
	prCapbsetDrop   = 24
	prCapAmbient    = 47
	capAmbientClear = 4

	seccompSetModeFilter = 1
	filterFlagListener   = 1 << 3

	ioctlNotifRecv = 0xC0502100
	ioctlNotifSend = 0xC0182101
	respFlagCont   = 1
)

type sockFprog struct {
	Len    uint16
	_      [6]byte
	Filter *Insn
}

// Run launches the entrypoint under the profile and waits for it. It returns
// the entrypoint's exit status (128+signal when killed). A *RefusalError means the
// reviewer was not started.
func Run(cfg Config) (int, error) {
	if os.Getuid() == 0 && cfg.DropTo == nil {
		return 0, refuse(CodePrivileged, "refusing to run the entrypoint as root; an unprivileged identity is required")
	}
	if !strings.HasPrefix(cfg.Entrypoint, "/") {
		return 0, refuse(CodeLaunchFailed, "entrypoint path must be absolute")
	}
	// The supervisor judges which exec is allowed. A same-uid reviewer must
	// not reach its memory or descriptors through /proc or pidfd_getfd, which
	// a seccomp filter on ptrace alone does not cover; a non-dumpable process
	// is refused by the same access check.
	if err := prctl(prSetDumpable, 0, 0); err != nil {
		return 0, refuse(CodePolicyUnenforceable, "making the supervisor non-dumpable: %v", err)
	}
	self, err := os.Executable()
	if err != nil {
		return 0, refuse(CodeLaunchFailed, "cannot locate the launcher image: %v", err)
	}
	sc, _ := json.Marshal(stageConfig{cfg.Entrypoint, cfg.SHA256, cfg.Argv, cfg.Dir, cfg.DropTo})

	pair, err := syscall.Socketpair(syscall.AF_UNIX, syscall.SOCK_STREAM|syscall.SOCK_CLOEXEC, 0)
	if err != nil {
		return 0, refuse(CodeLaunchFailed, "socketpair: %v", err)
	}
	parentSock, childSock := os.NewFile(uintptr(pair[0]), "sup"), os.NewFile(uintptr(pair[1]), "stage")
	statusR, statusW, err := os.Pipe()
	if err != nil {
		return 0, refuse(CodeLaunchFailed, "pipe: %v", err)
	}
	defer parentSock.Close()
	defer statusR.Close()

	cmd := exec.CommandContext(context.Background(), self, stageArg, string(sc)) //nolint:gosec // re-executes this very binary as the stage
	cmd.Env = []string{}
	cmd.Stdin, cmd.Stdout, cmd.Stderr = cfg.Stdin, cfg.Stdout, cfg.Stderr
	cmd.ExtraFiles = []*os.File{childSock, statusW}
	err = cmd.Start()
	_ = childSock.Close()
	_ = statusW.Close()
	if err != nil {
		return 0, refuse(CodeLaunchFailed, "starting the stage: %v", err)
	}

	listener, entryFD, recvErr := recvListener(int(parentSock.Fd()))
	var supDone chan struct{}
	if recvErr == nil {
		supDone = make(chan struct{})
		d := NewExecDecider(entryFD, HostArch())
		go func() { defer close(supDone); supervise(listener, d) }()
		_, _ = parentSock.Write([]byte{1})
	}
	waitErr := cmd.Wait()
	if recvErr == nil {
		_ = syscall.Close(listener)
		<-supDone
	}
	status, _ := io.ReadAll(statusR)
	if len(status) > 0 {
		var r RefusalError
		if json.Unmarshal(status, &r) == nil && r.Code != "" {
			return 0, &r
		}
	}
	if waitErr != nil {
		var ee *exec.ExitError
		if errors.As(waitErr, &ee) {
			if ws, ok := ee.Sys().(syscall.WaitStatus); ok && ws.Signaled() {
				return 128 + int(ws.Signal()), nil
			}
			return ee.ExitCode(), nil
		}
		return 0, refuse(CodeLaunchFailed, "waiting for the entrypoint: %v", waitErr)
	}
	return 0, nil
}

// recvListener receives the seccomp listener descriptor and the entrypoint
// descriptor number from the stage.
func recvListener(sock int) (listener, entryFD int, err error) {
	buf := make([]byte, 4)
	oob := make([]byte, syscall.CmsgSpace(4))
	n, oobn, _, _, err := syscall.Recvmsg(sock, buf, oob, 0)
	if err != nil {
		return 0, 0, err
	}
	if n != 4 {
		return 0, 0, io.EOF
	}
	msgs, err := syscall.ParseSocketControlMessage(oob[:oobn])
	if err != nil || len(msgs) == 0 {
		return 0, 0, io.EOF
	}
	fds, err := syscall.ParseUnixRights(&msgs[0])
	if err != nil || len(fds) != 1 {
		return 0, 0, io.EOF
	}
	return fds[0], int(binary.LittleEndian.Uint32(buf)), nil
}

// supervise answers notifications until the filter's last holder is gone.
func supervise(fd int, d *ExecDecider) {
	for {
		var req [80]byte
		if _, _, e := syscall.Syscall(syscall.SYS_IOCTL, uintptr(fd), ioctlNotifRecv, uintptr(unsafe.Pointer(&req[0]))); e != 0 { //nolint:gosec // G103: kernel ABI structs are passed by pointer
			if e == syscall.EINTR {
				continue
			}
			return
		}
		id := binary.LittleEndian.Uint64(req[0:8])
		nr := binary.LittleEndian.Uint32(req[16:20])
		var args [6]uint64
		for i := range args {
			args[i] = binary.LittleEndian.Uint64(req[32+8*i:])
		}
		pid := binary.LittleEndian.Uint32(req[8:12])
		dec := d.Decide(nr, args, pathIsEmpty(pid, args[1]))
		var resp [24]byte
		binary.LittleEndian.PutUint64(resp[0:8], id)
		if dec.Allow {
			binary.LittleEndian.PutUint32(resp[20:24], respFlagCont)
		} else {
			binary.LittleEndian.PutUint32(resp[16:20], uint32(-int32(dec.Errno)))
		}
		_, _, _ = syscall.Syscall(syscall.SYS_IOCTL, uintptr(fd), ioctlNotifSend, uintptr(unsafe.Pointer(&resp[0]))) //nolint:gosec // G103: kernel ABI structs are passed by pointer
	}
}

// MaybeStage runs the launcher's stage when this process was started as one,
// and never returns in that case; otherwise it returns false at once. Every
// binary that calls Run must call it first thing in main.
func MaybeStage() bool {
	if len(os.Args) != 3 || os.Args[1] != stageArg {
		return false
	}
	runtime.LockOSThread()
	status := os.NewFile(4, "status")
	var sc stageConfig
	if err := json.Unmarshal([]byte(os.Args[2]), &sc); err != nil {
		stageFail(status, refuse(CodeLaunchFailed, "bad stage config"))
	}
	stageFail(status, stage(sc, os.NewFile(3, "sup"), status))
	return true
}

func stageFail(status *os.File, err error) {
	var r *RefusalError
	if !errors.As(err, &r) {
		r = &RefusalError{Code: CodeLaunchFailed, Detail: err.Error()}
	}
	b, _ := json.Marshal(r)
	_, _ = status.Write(b)
	os.Exit(3)
}

// stage verifies, drops, filters and executes. It returns only on failure.
func stage(sc stageConfig, sock, status *os.File) error {
	syscall.CloseOnExec(3)
	syscall.CloseOnExec(4)
	// Whatever else the caller left open (a CI runner's or a supervising
	// shell's descriptors) must not follow the program through the exec.
	closeInherited()
	f, err := os.Open(sc.Entrypoint)
	if err != nil {
		return refuse(CodeLaunchFailed, "cannot open the entrypoint: %v", err)
	}
	entryFD := int(f.Fd()) // os.Open sets close-on-exec
	if err := VerifyEntrypoint(f, sc.SHA256); err != nil {
		return err
	}
	if sc.Dir != "" {
		if err := os.Chdir(sc.Dir); err != nil {
			return refuse(CodeLaunchFailed, "cannot enter the working directory: %v", err)
		}
	}
	if sc.DropTo != nil {
		if err := dropPrivileges(*sc.DropTo); err != nil {
			return err
		}
	}
	_ = prctl(prCapAmbient, capAmbientClear, 0)
	if e := prctl(prSetNoNewPrivs, 1, 0); e != nil {
		return refuse(CodePolicyUnenforceable, "no_new_privs: %v", e)
	}
	prog, err := BuildFilter(HostArch())
	if err != nil {
		return refuse(CodePolicyUnenforceable, "%v", err)
	}
	nr := sysSeccompAmd64
	if HostArch() == ArchArm64 {
		nr = sysSeccompArm64
	}
	fp := sockFprog{Len: uint16(len(prog)), Filter: &prog[0]}
	lfd, _, e := syscall.RawSyscall(uintptr(nr), seccompSetModeFilter, filterFlagListener, uintptr(unsafe.Pointer(&fp))) //nolint:gosec // G103: kernel ABI structs are passed by pointer
	if e != 0 {
		return refuse(CodePolicyUnenforceable, "seccomp filter with user notification: %v", e)
	}
	syscall.CloseOnExec(int(lfd))

	var num [4]byte
	binary.LittleEndian.PutUint32(num[:], uint32(entryFD))
	if err := syscall.Sendmsg(int(sock.Fd()), num[:], syscall.UnixRights(int(lfd)), nil, 0); err != nil {
		return refuse(CodeLaunchFailed, "handing the filter to the supervisor: %v", err)
	}
	ack := make([]byte, 1)
	if n, _ := sock.Read(ack); n != 1 {
		return refuse(CodeLaunchFailed, "supervisor did not confirm")
	}

	argv := append([]string{sc.Entrypoint}, sc.Argv...)
	argvp, err := syscall.SlicePtrFromStrings(argv)
	if err != nil {
		return refuse(CodeLaunchFailed, "bad argument: %v", err)
	}
	envp := []*byte{nil}
	empty := []byte{0}
	_, _, e = syscall.Syscall6(uintptr(Syscalls(HostArch()).Execveat), uintptr(entryFD), uintptr(unsafe.Pointer(&empty[0])), //nolint:gosec // G103: kernel ABI structs are passed by pointer
		uintptr(unsafe.Pointer(&argvp[0])), uintptr(unsafe.Pointer(&envp[0])), uintptr(AtEmptyPath), 0) //nolint:gosec // G103: pointers to live argv and envp arrays
	return refuse(CodeLaunchFailed, "executing the entrypoint: %v", e)
}

const prSetDumpable = 4

func prctl(opt, a, b uintptr) error {
	if _, _, e := syscall.RawSyscall6(syscall.SYS_PRCTL, opt, a, b, 0, 0, 0); e != 0 {
		return e
	}
	return nil
}

// dropPrivileges clears the capability bounding set and takes the identity.
func dropPrivileges(id Identity) error {
	last := 40
	if b, err := os.ReadFile("/proc/sys/kernel/cap_last_cap"); err == nil {
		if v, err := strconv.Atoi(strings.TrimSpace(string(b))); err == nil {
			last = v
		}
	}
	for c := 0; c <= last; c++ {
		if err := prctl(prCapbsetDrop, uintptr(c), 0); err != nil {
			return refuse(CodePolicyUnenforceable, "dropping capability %d: %v", c, err)
		}
	}
	if err := syscall.Setgroups(nil); err != nil {
		return refuse(CodePolicyUnenforceable, "clearing groups: %v", err)
	}
	if err := syscall.Setgid(id.GID); err != nil {
		return refuse(CodePolicyUnenforceable, "setgid: %v", err)
	}
	if err := syscall.Setuid(id.UID); err != nil {
		return refuse(CodePolicyUnenforceable, "setuid: %v", err)
	}
	return nil
}

// pathIsEmpty reads one byte at ptr in the stopped process and reports
// whether it is NUL. The stopped caller is the stage's exec thread; nothing
// else writes that buffer.
func pathIsEmpty(pid uint32, ptr uint64) bool {
	f, err := os.Open("/proc/" + strconv.FormatUint(uint64(pid), 10) + "/mem") //nolint:gosec // the pid comes from the kernel notification
	if err != nil {
		return false
	}
	defer func() { _ = f.Close() }()
	var b [1]byte
	n, err := f.ReadAt(b[:], int64(ptr)) //nolint:gosec // G115: a user pointer fits int64 on 64-bit targets
	return err == nil && n == 1 && b[0] == 0
}

// sysCloseRange is close_range(2); the number is the same on both architectures.
const (
	sysCloseRange     = 436
	closeRangeCloexec = 1 << 2
	firstInheritedFD  = 5 // 0-2 are the caller's streams, 3 and 4 the launcher's own
	maxFD             = ^uint(0)
)

// closeInherited marks every descriptor from firstInheritedFD up close-on-exec.
// Where close_range is missing it walks /proc/self/fd instead.
func closeInherited() {
	if _, _, e := syscall.RawSyscall(sysCloseRange, firstInheritedFD, uintptr(maxFD), closeRangeCloexec); e == 0 {
		return
	}
	ents, err := os.ReadDir("/proc/self/fd")
	if err != nil {
		return
	}
	for _, ent := range ents {
		if n, err := strconv.Atoi(ent.Name()); err == nil && n >= firstInheritedFD {
			syscall.CloseOnExec(n)
		}
	}
}
