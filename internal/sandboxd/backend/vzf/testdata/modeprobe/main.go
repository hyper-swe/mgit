// Command modeprobe is the GUEST half of the vzf mode-fidelity measurement
// (MGIT-81). libkrun's macOS virtio-fs presents a guest-written 0755 to the
// host as 0600, and the question this answers is whether
// Virtualization.framework's virtio-fs does the same — a question no amount of
// host-side reasoning settles, because the mapping lives inside the framework.
//
// The vzf guest image is deliberately minimal (no chmod, no stat, no coreutils
// at all), so the probe is a static binary the test drops into the shared
// worktree host-side and then execs INSIDE the guest: it creates each shape,
// reports the mode the guest itself sees, and lets the test compare that with
// its own host-side Lstat of the same inode.
// Refs: MGIT-81, MGIT-73, ADR-011
package main

import (
	"fmt"
	"os"
	"path/filepath"
	"syscall"
)

// probeCase is one create-a-file-and-look-at-it measurement. chmodTo exercises
// the path that matters most for artifacts: tar extraction and npm's bin
// linking set the executable bit with chmod AFTER the file exists.
type probeCase struct {
	name    string
	create  os.FileMode
	chmodTo os.FileMode
	isDir   bool
}

var probeCases = []probeCase{
	{name: "create-0755", create: 0o755},
	{name: "create-0644", create: 0o644},
	{name: "create-0600", create: 0o600},
	{name: "chmod-0755", create: 0o644, chmodTo: 0o755},
	{name: "dir-0755", create: 0o755, isDir: true},
}

func main() {
	if len(os.Args) < 2 {
		fmt.Println("MODEPROBE = FAILED (no target directory)")
		os.Exit(2)
	}
	root := os.Args[1]
	umask := syscall.Umask(0)
	syscall.Umask(umask)
	fmt.Printf("MODEPROBE umask = %04o\n", umask)
	if err := os.MkdirAll(root, 0o755); err != nil { //nolint:gosec // guest-side probe
		fmt.Printf("MODEPROBE = FAILED (%v)\n", err)
		os.Exit(1)
	}
	for _, c := range probeCases {
		got, err := run(root, c)
		if err != nil {
			fmt.Printf("MODEPROBE %s = FAILED (%v)\n", c.name, err)
			continue
		}
		fmt.Printf("MODEPROBE %s want=%04o got=%04o\n", c.name, c.want(os.FileMode(umask)), got)
	}
}

// want is the mode the case must end up with: a chmod is applied verbatim,
// while a create mode is masked by the umask first — that masking is the
// kernel behaving correctly, and folding it in keeps "want" an honest
// expectation rather than a wish.
func (c probeCase) want(umask os.FileMode) os.FileMode {
	if c.chmodTo != 0 {
		return c.chmodTo
	}
	return c.create &^ umask
}

// run performs one case and returns the permission bits the GUEST observes.
func run(root string, c probeCase) (os.FileMode, error) {
	path := filepath.Join(root, c.name)
	if c.isDir {
		if err := os.Mkdir(path, c.create); err != nil {
			return 0, err
		}
	} else if err := os.WriteFile(path, []byte("probe\n"), c.create); err != nil {
		return 0, err
	}
	if c.chmodTo != 0 {
		if err := os.Chmod(path, c.chmodTo); err != nil {
			return 0, err
		}
	}
	info, err := os.Lstat(path)
	if err != nil {
		return 0, err
	}
	return info.Mode().Perm(), nil
}
