// Command mgitrunner is the GUEST workload that proves the product claim by
// EXECUTION: inside a real libkrun microVM, mount the SEC-03 staged worktree
// at its identical path and run the real mgit CLI against the private store
// delivered there.
//
// It stands in for mgit-guest's PID-1 duties for this one measurement — mount
// the virtio-fs worktree tag, then run a workload in it — because what is
// being proven is that mgit works in the guest, not the exec/vsock control
// plane (covered separately). Refs: MGIT-61.7, SEC-03, FR-17.3
package main

import (
	"fmt"
	"os"
	"os/exec"
	"strings"

	"golang.org/x/sys/unix"
)

// The host passes these in the guest environment, using the same boot-token
// contract every backend uses (guestboot); read directly here to keep this
// workload free of repo imports.
const (
	envBootTokens = "MGIT_GUEST_BOOT"
	keyPath       = "mgit.worktree"
	keyFS         = "mgit.worktree_fs"
	keySource     = "mgit.worktree_src"
)

// token extracts one space-separated key=value from the boot tokens.
func token(tokens, key string) string {
	for _, f := range strings.Fields(tokens) {
		if k, v, ok := strings.Cut(f, "="); ok && k == key {
			return v
		}
	}
	return ""
}

// taskFromStore reads the task the private store is bound to, from the branch
// its HEAD points at ("ref: refs/heads/task/<ID>"). That is how the guest
// genuinely knows its task — the host does not need to pass it separately.
func taskFromStore(wtPath string) string {
	b, err := os.ReadFile(wtPath + "/.mgit/HEAD") //nolint:gosec // guest store path
	if err != nil {
		return ""
	}
	ref := strings.TrimSpace(strings.TrimPrefix(string(b), "ref:"))
	return strings.TrimPrefix(strings.TrimSpace(ref), "refs/heads/task/")
}

// runMgit executes the in-guest mgit CLI in dir and reports the outcome.
func runMgit(dir string, args ...string) (string, error) {
	cmd := exec.Command("/bin/mgit", args...) //nolint:gosec // fixed guest path
	cmd.Dir = dir
	cmd.Env = []string{
		"PATH=/bin:/sbin",
		"HOME=/tmp",
		// The marker mgit reads to refuse host-only verbs in-guest.
		"MGIT_IN_SANDBOX=1",
	}
	out, err := cmd.CombinedOutput()
	return strings.TrimSpace(string(out)), err
}

func main() {
	fmt.Println("GUEST: booted inside a real libkrun microVM")

	if err := unix.Mount("proc", "/proc", "proc", 0, ""); err != nil {
		fmt.Printf("GUEST: mount /proc: %v\n", err)
	}

	tokens := os.Getenv(envBootTokens)
	wtPath, wtFS, wtSrc := token(tokens, keyPath), token(tokens, keyFS), token(tokens, keySource)
	if wtPath == "" || wtFS == "" || wtSrc == "" {
		fmt.Printf("GUEST-RESULT MOUNT = FAILED (incomplete boot tokens %q)\n", tokens)
		fmt.Println("GUEST: done")
		return
	}

	// The worktree is delivered at its IDENTICAL host path (FR-17.3).
	if err := os.MkdirAll(wtPath, 0o755); err != nil { //nolint:gosec // guest mount point
		fmt.Printf("GUEST-RESULT MOUNT = FAILED (mkdir: %v)\n", err)
		fmt.Println("GUEST: done")
		return
	}
	if err := unix.Mount(wtSrc, wtPath, wtFS, 0, ""); err != nil {
		fmt.Printf("GUEST-RESULT MOUNT = FAILED (mount %s %s at %s: %v)\n", wtSrc, wtFS, wtPath, err)
		fmt.Println("GUEST: done")
		return
	}
	fmt.Printf("GUEST-RESULT MOUNT = OK (%s at %s)\n", wtSrc, wtPath)

	// The store the guest sees must be the PRIVATE one delivered with the
	// staged tree, never the host's shared store.
	if _, err := os.Stat(wtPath + "/.mgit/HEAD"); err != nil {
		fmt.Printf("GUEST-RESULT STORE = MISSING (%v)\n", err)
		fmt.Println("GUEST: done")
		return
	}
	fmt.Println("GUEST-RESULT STORE = PRESENT")

	if out, err := runMgit(wtPath, "log"); err != nil {
		fmt.Printf("GUEST-RESULT LOG = FAILED (%v) %s\n", err, out)
	} else {
		fmt.Printf("GUEST-RESULT LOG = OK first_line=%q\n", strings.SplitN(out, "\n", 2)[0])
	}

	// THE product claim: an agent commits from inside the sandbox.
	if err := os.WriteFile(wtPath+"/agent-change.txt",
		[]byte("written by the agent inside the sandbox\n"), 0o600); err != nil {
		fmt.Printf("GUEST-RESULT WRITE = FAILED (%v)\n", err)
		fmt.Println("GUEST: done")
		return
	}
	if out, err := runMgit(wtPath, "add", "agent-change.txt"); err != nil {
		fmt.Printf("GUEST-RESULT ADD = FAILED (%v) %s\n", err, out)
		fmt.Println("GUEST: done")
		return
	}
	out, err := runMgit(wtPath, "commit", "-m", "agent commit inside the sandbox", "--task", taskFromStore(wtPath))
	if err != nil {
		fmt.Printf("GUEST-RESULT COMMIT = FAILED (%v) %s\n", err, out)
		fmt.Println("GUEST: done")
		return
	}
	fmt.Printf("GUEST-RESULT COMMIT = OK %s\n", out)

	// A host-only verb must refuse with a diagnosis, not a socket error.
	if out, err := runMgit(wtPath, "sandbox", "list"); err == nil {
		fmt.Printf("GUEST-RESULT HOSTONLY = NOT-REFUSED %s\n", out)
	} else if strings.Contains(out, "inside the mgit sandbox") {
		fmt.Println("GUEST-RESULT HOSTONLY = REFUSED-WITH-REASON")
	} else {
		fmt.Printf("GUEST-RESULT HOSTONLY = REFUSED-UNCLEAR %s\n", out)
	}

	fmt.Println("GUEST: done")
}
