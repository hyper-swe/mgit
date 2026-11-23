//go:build linux

package reviewlaunch

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"unsafe"

	"golang.org/x/sys/unix"
)

var emptyStr = []byte{0}

// TestMain lets this test binary play three roles: the test run itself, the
// launcher's re-exec'd stage (MaybeStage), and the mapped entrypoint (probe).
func TestMain(m *testing.M) {
	if MaybeStage() {
		return
	}
	if len(os.Args) > 1 && os.Args[1] == "--probe" {
		probe(os.Args[2], os.Args[3:])
		return
	}
	os.Exit(m.Run())
}

//nolint:gosec // the probe deliberately attempts the writes and launches the profile must refuse
func probe(mode string, extra []string) {
	switch mode {
	case "ok":
		fmt.Println("hello from the mapped entrypoint")
	case "status":
		b, _ := os.ReadFile("/proc/self/status")
		for _, l := range strings.Split(string(b), "\n") {
			for _, k := range []string{"NoNewPrivs", "Seccomp:", "CapBnd", "CapEff", "Uid"} {
				if strings.HasPrefix(l, k) {
					fmt.Println(strings.Join(strings.Fields(l), " "))
				}
			}
		}
	case "supervisor-mem":
		f, err := os.OpenFile(fmt.Sprintf("/proc/%d/mem", os.Getppid()), os.O_RDONLY, 0)
		if err == nil {
			_ = f.Close()
		}
		fmt.Printf("supervisor-mem: %v\n", err)
	case "write-attempts":
		dir := extra[0]
		marker := dir + "/review-marker"
		report := func(name string, err error) { fmt.Printf("%s: %v\n", name, err) }
		_, err := os.OpenFile(marker, os.O_WRONLY|os.O_CREATE, 0o600)
		report("create", err)
		report("mkdir", os.Mkdir(dir+"/review-dir", 0o700))
		report("symlink", os.Symlink("/etc/passwd", dir+"/review-link"))
		report("rename", os.Rename("/proc/self/exe", dir+"/review-moved"))
		report("remove", os.Remove("/etc/hostname"))
		report("truncate", os.Truncate("/etc/hostname", 0))
		report("chmod", os.Chmod("/etc/hostname", 0o777))
		_, err = os.ReadFile("/etc/hostname")
		report("read", err)
		_, err = os.OpenFile("/etc/hostname", os.O_RDONLY, 0)
		report("open-read", err)
		_, statErr := os.Stat(marker)
		report("marker-exists", statErr)
		fmt.Println("stdout still works")
	case "fds":
		ents, _ := os.ReadDir("/proc/self/fd")
		for _, e := range ents {
			target, _ := os.Readlink("/proc/self/fd/" + e.Name())
			info, _ := os.ReadFile("/proc/self/fdinfo/" + e.Name())
			flags := ""
			for _, l := range strings.Split(string(info), "\n") {
				if strings.HasPrefix(l, "flags:") {
					flags = strings.TrimSpace(strings.TrimPrefix(l, "flags:"))
				}
			}
			fmt.Printf("fd %s -> %s flags %s\n", e.Name(), target, flags)
		}
	case "ioctl-setflags":
		f, err := os.Open(extra[0])
		if err != nil {
			fmt.Printf("ioctl-setflags: open %v\n", err)
			return
		}
		flags := uint32(0)
		_, _, e := syscall.Syscall(syscall.SYS_IOCTL, f.Fd(), 0x40086602, uintptr(unsafe.Pointer(&flags))) //nolint:gosec // G103: kernel ABI request
		fmt.Printf("ioctl-setflags: %v\n", e)
	case "mq-create":
		name := append([]byte("/review-mq-probe"), 0)
		_, _, e := syscall.Syscall6(syscall.SYS_MQ_OPEN, uintptr(unsafe.Pointer(&name[0])), uintptr(syscall.O_CREAT|syscall.O_RDWR), 0o600, 0, 0, 0) //nolint:gosec // G103: kernel ABI string
		fmt.Printf("mq-create: %v\n", e)
	case "path-exec":
		err := syscall.Exec("/bin/true", []string{"true"}, nil)
		fmt.Printf("path-exec: %v\n", err)
	case "fork":
		// ForkExec with explicit descriptors opens nothing itself, so the
		// refusal seen is the fork's, not an open of /dev/null.
		_, err := syscall.ForkExec("/bin/true", []string{"true"}, &syscall.ProcAttr{Files: []uintptr{0, 1, 2}})
		fmt.Printf("fork: %v\n", err)
	case "second-exec":
		// The one allowed exec already happened (we are its result); an
		// execveat on our own image by fd must now be refused.
		f, err := os.Open("/proc/self/exe")
		if err != nil {
			fmt.Printf("second-exec: open %v\n", err)
			return
		}
		_, _, e := syscall.Syscall6(uintptr(Syscalls(HostArch()).Execveat), f.Fd(), uintptr(unsafe.Pointer(&emptyStr[0])), 0, 0, uintptr(AtEmptyPath), 0) //nolint:gosec // G103: raw execveat in a probe
		fmt.Printf("second-exec: %v\n", e)
	case "thread":
		done := make(chan struct{})
		go func() { close(done) }()
		<-done
		fmt.Println("thread: ok")
	}
}

// stageBinary copies this test binary to a world-readable place so the dropped
// identity can run it, and returns the path and its digest.
func stageBinary(t *testing.T) (string, string) {
	t.Helper()
	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(self) //nolint:gosec // test: this test binary
	if err != nil {
		t.Fatal(err)
	}
	dir, err := os.MkdirTemp("", "reviewlaunch")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Remove(filepath.Join(dir, "entry")); _ = os.Remove(dir) })
	if err := os.Chmod(dir, 0o755); err != nil { //nolint:gosec // test: the dropped identity must reach the binary
		t.Fatal(err)
	}
	p := filepath.Join(dir, "entry")
	if err := os.WriteFile(p, b, 0o755); err != nil { //nolint:gosec // test: an executable copy
		t.Fatal(err)
	}
	return p, sum(b)
}

func runProbe(t *testing.T, mode string, extra ...string) (string, int, error) {
	t.Helper()
	p, d := stageBinary(t)
	var out bytes.Buffer
	cfg := Config{Entrypoint: p, SHA256: d, Argv: append([]string{"--probe", mode}, extra...), Stdout: &out, Stderr: io.Discard}
	if os.Getuid() == 0 {
		cfg.DropTo = &Identity{UID: 65534, GID: 65534}
	}
	code, err := Run(cfg)
	return out.String(), code, err
}

func skipIfNoFilter(t *testing.T, err error) {
	t.Helper()
	var r *RefusalError
	// Only a kernel that lacks the feature skips; any other refusal (a
	// permission error, say) is the launcher failing and must fail the test.
	if errors.As(err, &r) && r.Code == CodePolicyUnenforceable &&
		(strings.Contains(r.Detail, "invalid argument") || strings.Contains(r.Detail, "function not implemented")) {
		t.Skipf("seccomp user notification unavailable here: %v", err)
	}
}

func TestRun_MappedEntrypointRuns(t *testing.T) {
	out, code, err := runProbe(t, "ok")
	skipIfNoFilter(t, err)
	if err != nil || code != 0 || !strings.Contains(out, "hello from the mapped entrypoint") {
		t.Fatalf("code=%d err=%v out=%q", code, err, out)
	}
}

func TestRun_ChildSeesTheEnforcedState(t *testing.T) {
	out, _, err := runProbe(t, "status")
	skipIfNoFilter(t, err)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"NoNewPrivs: 1", "Seccomp: 2", "CapEff: 0000000000000000"} {
		if !strings.Contains(out, want) {
			t.Errorf("status lacks %q:\n%s", want, out)
		}
	}
	if os.Getuid() == 0 {
		for _, want := range []string{"Uid: 65534 65534 65534 65534", "CapBnd: 0000000000000000"} {
			if !strings.Contains(out, want) {
				t.Errorf("not dropped, lacks %q:\n%s", want, out)
			}
		}
	}
}

func TestRun_FurtherLaunchesAreDenied(t *testing.T) {
	for mode, want := range map[string]string{
		"path-exec":   "path-exec: operation not permitted",
		"fork":        "fork: operation not permitted",
		"second-exec": "second-exec: operation not permitted",
	} {
		out, _, err := runProbe(t, mode)
		skipIfNoFilter(t, err)
		if err != nil || !strings.Contains(out, want) {
			t.Errorf("%s: err=%v out=%q, want %q", mode, err, out, want)
		}
	}
}

func TestRun_ThreadsStillWork(t *testing.T) {
	out, _, err := runProbe(t, "thread")
	skipIfNoFilter(t, err)
	if err != nil || !strings.Contains(out, "thread: ok") {
		t.Fatalf("err=%v out=%q", err, out)
	}
}

func TestRun_HashMismatchStartsNothing(t *testing.T) {
	p, _ := stageBinary(t)
	var out bytes.Buffer
	cfg := Config{Entrypoint: p, SHA256: sum([]byte("not the binary")), Argv: []string{"--probe", "ok"}, Stdout: &out, Stderr: io.Discard}
	if os.Getuid() == 0 {
		cfg.DropTo = &Identity{UID: 65534, GID: 65534}
	}
	_, err := Run(cfg)
	var r *RefusalError
	if !errors.As(err, &r) || r.Code != CodeHashMismatch {
		t.Fatalf("err %v, want HASH_MISMATCH", err)
	}
	if out.Len() != 0 {
		t.Errorf("the entrypoint produced output after a refusal: %q", out.String())
	}
}

func TestRun_RootWithoutIdentityIsRefused(t *testing.T) {
	if os.Getuid() != 0 {
		t.Skip("only meaningful as root")
	}
	p, d := stageBinary(t)
	_, err := Run(Config{Entrypoint: p, SHA256: d, Argv: []string{"--probe", "ok"}, Stdout: io.Discard, Stderr: io.Discard})
	var r *RefusalError
	if !errors.As(err, &r) || r.Code != CodePrivileged {
		t.Fatalf("err %v, want PRIVILEGED", err)
	}
}

// The supervisor decides which exec is allowed; a same-uid reviewer must not
// be able to rewrite its memory through /proc or take its descriptors.
func TestRun_SupervisorIsNotDumpable(t *testing.T) {
	out, _, err := runProbe(t, "supervisor-mem")
	skipIfNoFilter(t, err)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "supervisor-mem: open") || !strings.Contains(out, "permission denied") {
		t.Fatalf("the reviewer could open the supervisor's memory: %q", out)
	}
	r1, _, e := syscall.RawSyscall(syscall.SYS_PRCTL, 3 /* PR_GET_DUMPABLE */, 0, 0)
	if e != 0 || r1 != 0 {
		t.Fatalf("supervisor dumpable=%d err=%v, want 0", r1, e)
	}
}

func TestYamaScope_ReadsTheKernelValue(t *testing.T) {
	got := YamaScope()
	if got == "" {
		t.Fatal("YamaScope returned empty; an unreadable value must say unknown")
	}
	if raw, err := os.ReadFile("/proc/sys/kernel/yama/ptrace_scope"); err == nil {
		if want := strings.TrimSpace(string(raw)); got != want {
			t.Fatalf("YamaScope=%q want %q", got, want)
		}
	} else if got != "unknown" {
		t.Fatalf("no yama here, want unknown, got %q", got)
	}
}

func TestRun_FileWritesAreDenied(t *testing.T) {
	out, _, err := runProbe(t, "write-attempts", t.TempDir())
	skipIfNoFilter(t, err)
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"create", "mkdir", "symlink", "rename", "remove", "truncate", "chmod"} {
		if !strings.Contains(out, name+": ") || !strings.Contains(lineOf(out, name+": "), "operation not permitted") {
			t.Errorf("%s was not refused with EPERM:\n%s", name, out)
		}
	}
	if !strings.Contains(lineOf(out, "read: "), "<nil>") || !strings.Contains(lineOf(out, "open-read: "), "<nil>") {
		t.Errorf("reading must still work:\n%s", out)
	}
	if !strings.Contains(lineOf(out, "marker-exists: "), "no such file") {
		t.Errorf("a marker file exists after a refused create:\n%s", out)
	}
	if !strings.Contains(out, "stdout still works") {
		t.Errorf("stdout is broken:\n%s", out)
	}
}

func lineOf(out, prefix string) string {
	for _, l := range strings.Split(out, "\n") {
		if strings.HasPrefix(l, prefix) {
			return l
		}
	}
	return ""
}

// The program holds no file descriptor it could write through except what the
// caller handed it as standard input, output and error: every descriptor the
// launcher used is closed before the program starts, and the filter makes it
// impossible to open a new file for writing. What remains beyond the standard
// streams is the probe's own (the directory it lists, the runtime's poller),
// and none of it is a path to a file.
func TestRun_HoldsNoFileDescriptorsBeyondStandardStreams(t *testing.T) {
	// A descriptor the caller itself leaked (open, writable, not close-on-exec),
	// as a CI runner or a supervising shell can: it must not reach the program.
	r, w, perr := os.Pipe()
	if perr != nil {
		t.Fatal(perr)
	}
	defer r.Close()
	defer w.Close()
	if _, _, e := syscall.Syscall(syscall.SYS_FCNTL, w.Fd(), syscall.F_SETFD, 0); e != 0 {
		t.Fatal(e)
	}
	// An append-mode regular file leaked the same way: writing through it
	// changes a file, so it must not reach the program either.
	af, aerr := os.OpenFile(t.TempDir()+"/leak", os.O_WRONLY|os.O_CREATE|os.O_APPEND, 0o600)
	if aerr != nil {
		t.Fatal(aerr)
	}
	defer af.Close()
	if _, _, e := syscall.Syscall(syscall.SYS_FCNTL, af.Fd(), syscall.F_SETFD, 0); e != 0 {
		t.Fatal(e)
	}
	out, _, err := runProbe(t, "fds")
	skipIfNoFilter(t, err)
	if err != nil {
		t.Fatal(err)
	}
	seen := 0
	for _, l := range strings.Split(out, "\n") {
		var fd, target, flagWord, flags string
		if n, _ := fmt.Sscanf(l, "fd %s -> %s %s %s", &fd, &target, &flagWord, &flags); n < 4 {
			continue
		}
		seen++
		if fd == "0" || fd == "1" || fd == "2" || strings.HasPrefix(target, "anon_inode:") {
			continue
		}
		// Every other descriptor is read-only: access mode bits (the low two
		// bits of the octal flags) are zero.
		v, err := strconv.ParseUint(flags, 8, 64)
		if err != nil || v&3 != 0 {
			t.Errorf("descriptor %s -> %s has flags %s: it is not read-only", fd, target, flags)
		}
	}
	if seen < 3 {
		t.Fatalf("the probe listed %d descriptors:\n%s", seen, out)
	}
}

func TestRun_InodeFlagIoctlIsRefused(t *testing.T) {
	f := t.TempDir() + "/target"
	if err := os.WriteFile(f, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	out, _, err := runProbe(t, "ioctl-setflags", f)
	skipIfNoFilter(t, err)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "ioctl-setflags: operation not permitted") {
		t.Fatalf("the inode flags request was not refused by the profile: %q", out)
	}
}

func TestRun_MessageQueueCreateIsRefused(t *testing.T) {
	out, _, err := runProbe(t, "mq-create")
	skipIfNoFilter(t, err)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "mq-create: operation not permitted") {
		t.Fatalf("message queue creation was not refused: %q", out)
	}
}

// The launcher must not start a program with descriptors it could not mark.
func TestCloseInherited_RefusesWhenNothingCanMarkThem(t *testing.T) {
	restore := stubFDHooks()
	defer restore()
	closeRangeCall = func(_, _, _ uintptr) error { return syscall.ENOSYS }
	listFDs = func() ([]string, error) { return nil, errors.New("no /proc") }
	err := closeInherited()
	var r *RefusalError
	if !errors.As(err, &r) || r.Code != CodePolicyUnenforceable {
		t.Fatalf("closeInherited with both ways broken: %v, want a POLICY_UNENFORCEABLE refusal", err)
	}
}

// The first descriptor marked is exactly 5: 0-2 are the caller's streams, 3
// and 4 the launcher's own.
func TestCloseInherited_StartsAtExactlyFive(t *testing.T) {
	restore := stubFDHooks()
	defer restore()
	var first, flags uintptr
	closeRangeCall = func(f, _, fl uintptr) error { first, flags = f, fl; return nil }
	if err := closeInherited(); err != nil {
		t.Fatal(err)
	}
	if first != 5 || flags != 1<<2 {
		t.Fatalf("close_range started at %d flags %#x, want 5 and CLOSE_RANGE_CLOEXEC", first, flags)
	}
	// The same boundary on the fallback path.
	closeRangeCall = func(_, _, _ uintptr) error { return syscall.ENOSYS }
	listFDs = func() ([]string, error) { return []string{"0", "2", "3", "4", "5", "6", "x"}, nil }
	var marked []int
	setCloexec = func(fd int) { marked = append(marked, fd) }
	if err := closeInherited(); err != nil {
		t.Fatal(err)
	}
	if fmt.Sprint(marked) != "[5 6]" {
		t.Fatalf("fallback marked %v, want [5 6]", marked)
	}
}

func stubFDHooks() func() {
	a, b, c := closeRangeCall, listFDs, setCloexec
	return func() { closeRangeCall, listFDs, setCloexec = a, b, c }
}

// If the inherited descriptors cannot be marked, the stage refuses before it
// opens the entrypoint or starts anything. The entrypoint here does not exist:
// a stage that ignored the failure would reach it and report LAUNCH_FAILED.
func TestStage_RefusesWhenInheritedDescriptorsCannotBeMarked(t *testing.T) {
	restore := stubFDHooks()
	defer restore()
	closeRangeCall = func(_, _, _ uintptr) error { return syscall.ENOSYS }
	listFDs = func() ([]string, error) { return nil, errors.New("no /proc") }
	err := stage(stageConfig{Entrypoint: "/nonexistent/entrypoint", SHA256: strings.Repeat("0", 64)}, nil, nil)
	var r *RefusalError
	if !errors.As(err, &r) || r.Code != CodePolicyUnenforceable {
		t.Fatalf("stage err %v, want POLICY_UNENFORCEABLE", err)
	}
}

// A descriptor far above the usual range must not follow the program either:
// the range that is marked has to reach the top, not stop at an arbitrary
// number.
func TestRun_AHighLeakedDescriptorDoesNotReachTheProgram(t *testing.T) {
	const high = 60000
	var lim syscall.Rlimit
	if err := syscall.Getrlimit(syscall.RLIMIT_NOFILE, &lim); err != nil {
		t.Fatal(err)
	}
	if lim.Max < high+16 {
		t.Skipf("hard descriptor limit %d is below %d", lim.Max, high+16)
	}
	if lim.Cur < high+16 {
		old := lim
		lim.Cur = high + 16
		if err := syscall.Setrlimit(syscall.RLIMIT_NOFILE, &lim); err != nil {
			t.Skipf("cannot raise the descriptor limit: %v", err)
		}
		defer func() { _ = syscall.Setrlimit(syscall.RLIMIT_NOFILE, &old) }()
	}
	af, err := os.OpenFile(t.TempDir()+"/leak", os.O_WRONLY|os.O_CREATE|os.O_APPEND, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	defer af.Close()
	if err := unix.Dup3(int(af.Fd()), high, 0); err != nil {
		t.Skipf("cannot place a descriptor at %d: %v", high, err)
	}
	defer func() { _ = syscall.Close(high) }()
	out, _, perr := runProbe(t, "fds")
	skipIfNoFilter(t, perr)
	if perr != nil {
		t.Fatal(perr)
	}
	for _, l := range strings.Split(out, "\n") {
		var fd string
		if n, _ := fmt.Sscanf(l, "fd %s", &fd); n == 1 && fd == strconv.Itoa(high) {
			t.Fatalf("descriptor %d reached the program:\n%s", high, out)
		}
	}
}
