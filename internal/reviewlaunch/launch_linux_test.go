//go:build linux

package reviewlaunch

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"unsafe"
)

var emptyStr = []byte{0}

// TestMain lets this test binary play three roles: the test run itself, the
// launcher's re-exec'd stage (MaybeStage), and the mapped entrypoint (probe).
func TestMain(m *testing.M) {
	if MaybeStage() {
		return
	}
	if len(os.Args) > 1 && os.Args[1] == "--probe" {
		probe(os.Args[2])
		return
	}
	os.Exit(m.Run())
}

func probe(mode string) {
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
		f, err := os.OpenFile(fmt.Sprintf("/proc/%d/mem", os.Getppid()), os.O_RDWR, 0)
		if err == nil {
			_ = f.Close()
		}
		fmt.Printf("supervisor-mem: %v\n", err)
	case "path-exec":
		err := syscall.Exec("/bin/true", []string{"true"}, nil)
		fmt.Printf("path-exec: %v\n", err)
	case "fork":
		err := exec.Command("/bin/true").Run()
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

func runProbe(t *testing.T, mode string) (string, int, error) {
	t.Helper()
	p, d := stageBinary(t)
	var out bytes.Buffer
	cfg := Config{Entrypoint: p, SHA256: d, Argv: []string{"--probe", mode}, Stdout: &out, Stderr: io.Discard}
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
		"fork":        "fork: fork/exec /bin/true: operation not permitted",
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
