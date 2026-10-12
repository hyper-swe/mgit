//go:build cgo && !vzf && (darwin || (linux && libkrun))

package libkrun

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/hyper-swe/mgit/internal/model"
)

// TestE2E_Libkrun_RealVM_ListingADanglingLinkInTheShare_PrintsNoError is
// MGIT-225's receipt: listing a shared tree that holds a dangling symlink
// prints nothing on stderr inside the guest.
//
// GNU `ls -l` reads each entry's attribute names without following it. The
// share answered that question for a dangling link by following the link to
// its missing target, ENOENT, and ls printed "ls: <link>: No such file or
// directory" over a correct listing. The fix is mgit's patched libkrun (the
// darwin bundle, MGIT-259). Stock and patched libkrun are the same release,
// so the same test is both halves of the proof:
//
//   - against stock libkrun it is RED (this is the delete-subject: the build
//     without the patch);
//   - against the patched bundle it is GREEN. Point the loader at the bundle:
//     DYLD_LIBRARY_PATH=<bundle>/lib MGIT_E2E_LIBKRUN=1 /tmp/libkrun.test \
//     -test.run TestE2E_Libkrun_RealVM_ListingADanglingLinkInTheShare
//
// A control asks the same question on a guest-local tmpfs, which answers on
// every build, so a red result is never a probe that cannot answer.
// Gated like every real-VM test (see e2e_realvm_test.go). Refs: MGIT-225, MGIT-259
func TestE2E_Libkrun_RealVM_ListingADanglingLinkInTheShare_PrintsNoError(t *testing.T) {
	requireRealVM(t)
	guestRoot := buildGuestWorkload(t, "xattrprobe")

	wt := t.TempDir()
	src := filepath.Join(wt, "src")
	if err := os.MkdirAll(src, 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(src, "a.go"), []byte("package a\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	for name, target := range map[string]string{"good": "a.go", "dangling": "missing.go"} {
		if err := os.Symlink(target, filepath.Join(src, name)); err != nil {
			t.Fatal(err)
		}
	}

	cfg := realVMConfig(t, guestRoot, model.NetworkModeNone, nil)
	cfg.RootfsReadOnly = false
	cfg.WorktreePath = wt
	cfg.WorktreeTag = "work"
	cfg.VsockEnabled = false
	console := bootVM(t, cfg)

	if !strings.Contains(console, "PROBE control/tmp/dangling llistxattr=") ||
		strings.Contains(console, "LS-STDERR ls: control/") {
		t.Fatalf("the control could not answer, so this run proves nothing; console:\n%s", console)
	}
	for _, entry := range []string{"src/a.go", "src/good", "src/dangling", "src/guestdangling"} {
		if !strings.Contains(console, "PROBE "+entry+" llistxattr=") {
			t.Fatalf("the guest never asked about %s; console:\n%s", entry, console)
		}
	}
	var stray []string
	for _, line := range strings.Split(console, "\n") {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, "PROBE ") {
			t.Log(line) // the guest's answers are the receipt, pass or fail
		}
		if strings.HasPrefix(line, "LS-STDERR ") {
			stray = append(stray, line)
		}
	}
	if len(stray) > 0 {
		t.Errorf("listing the share printed on stderr (the share followed a dangling link):\n  %s\nconsole:\n%s",
			strings.Join(stray, "\n  "), console)
	}
}
