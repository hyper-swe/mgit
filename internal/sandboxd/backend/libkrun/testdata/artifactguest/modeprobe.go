// Mode-fidelity probe (MGIT-81). MGIT-73 measured that a file the guest writes
// 0755 is presented to the HOST on the virtio-fs share as 0600, but "the guest
// wrote 0755" was an INTENT, not a measurement: nothing had ever stat'd the
// file from inside the VM. This probe closes that gap so the loss can be
// attributed to one of the three candidates — the guest's umask, the way the
// workload writes the file, or libkrun's virtio-fs — instead of inferred.
//
// It writes the same shapes twice: once onto a guest-private tmpfs (the
// CONTROL, which no virtio-fs code touches) and once onto the shared worktree
// (the SUBJECT). Every result is printed as a machine-parseable console line
// the host-side test parses and compares against its own Lstat of the share.
// Refs: MGIT-81, MGIT-73, ADR-011

package main

import (
	"fmt"
	"os"
	"path/filepath"

	"golang.org/x/sys/unix"
)

// modeProbeDir is the subtree the probe writes into on the share. It sits
// beside the exported artifact rather than inside it so the export e2e's own
// file counts are unaffected.
const modeProbeDir = "modeprobe"

// tmpfsControlDir is the guest-private mount point for the control arm.
const tmpfsControlDir = "/tmp/modeprobe"

// probeCase is one create-a-file-and-look-at-it measurement.
type probeCase struct {
	name    string // stable label the host-side test keys on
	create  os.FileMode
	chmodTo os.FileMode // 0 = do not chmod
	isDir   bool
}

// probeCases cover the shapes an artifact actually contains: an executable, a
// plain data file, a private file, a directory, and — the interesting one — a
// file whose mode is set by chmod AFTER creation rather than by the create
// mode, which is how tar extraction and npm's bin linking set the bit.
var probeCases = []probeCase{
	{name: "create-0755", create: 0o755},
	{name: "create-0644", create: 0o644},
	{name: "create-0600", create: 0o600},
	{name: "create-0777", create: 0o777},
	{name: "chmod-0755", create: 0o644, chmodTo: 0o755},
	{name: "dir-0755", create: 0o755, isDir: true},
}

// probeModes runs every case on the tmpfs control and on the shared worktree
// and reports each observed mode. Failures are reported rather than fatal: a
// probe that cannot run must not mask the export e2e it rides along with.
func probeModes(wt string) {
	umask := unix.Umask(0)
	unix.Umask(umask)
	fmt.Printf("GUEST-MODE umask = %04o\n", umask)

	if err := mountTmpfsControl(); err != nil {
		fmt.Printf("GUEST-MODE control = UNAVAILABLE (%v)\n", err)
	} else {
		runProbeArm("tmpfs", tmpfsControlDir, os.FileMode(umask))
	}
	runProbeArm("share", filepath.Join(wt, modeProbeDir), os.FileMode(umask))
}

// mountTmpfsControl gives the probe a filesystem libkrun's virtio-fs has
// nothing to do with, so "the guest asked for 0755 and got 0755" can be
// established independently of the share.
func mountTmpfsControl() error {
	if err := os.MkdirAll(tmpfsControlDir, 0o755); err != nil { //nolint:gosec // guest-side probe
		return err
	}
	return unix.Mount("tmpfs", tmpfsControlDir, "tmpfs", 0, "")
}

// runProbeArm executes every case under one root and prints what the guest
// itself sees afterwards.
func runProbeArm(arm, root string, umask os.FileMode) {
	if err := os.MkdirAll(root, 0o755); err != nil { //nolint:gosec // guest-side probe
		fmt.Printf("GUEST-MODE %s = FAILED (%v)\n", arm, err)
		return
	}
	for _, c := range probeCases {
		got, err := runProbeCase(root, c)
		if err != nil {
			fmt.Printf("GUEST-MODE %s %s = FAILED (%v)\n", arm, c.name, err)
			continue
		}
		fmt.Printf("GUEST-MODE %s %s want=%04o got=%04o\n", arm, c.name, c.want(umask), got)
	}
}

// want is the mode the case must end up with: a chmod is applied verbatim,
// while a create mode is masked by the umask first — that masking is the
// kernel behaving correctly, and folding it in here keeps the probe's "want"
// an honest expectation rather than a wish.
func (c probeCase) want(umask os.FileMode) os.FileMode {
	if c.chmodTo != 0 {
		return c.chmodTo
	}
	return c.create &^ umask
}

// runProbeCase performs one case and returns the permission bits the GUEST
// observes for the resulting inode.
func runProbeCase(root string, c probeCase) (os.FileMode, error) {
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
