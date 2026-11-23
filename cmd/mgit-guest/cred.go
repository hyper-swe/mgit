package main

import (
	"flag"
	"fmt"
	"io"
	"math"
	"os"
	"path/filepath"
	"regexp"

	"github.com/hyper-swe/mgit/internal/model"
)

// credDir is where a review credential is placed: a tmpfs the guest mounts at
// boot, so the file never reaches a disk image.
const credDir = "/run/review-cred" //nolint:gosec // a directory path, not a credential

var credName = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,63}$`)

// credDest checks that dest is one plain name directly inside root.
func credDest(root, dest string) error {
	if !filepath.IsAbs(dest) || filepath.Clean(dest) != dest || filepath.Dir(dest) != root || !credName.MatchString(filepath.Base(dest)) {
		return fmt.Errorf("the destination must be one plain name inside %s", root)
	}
	return nil
}

// credInstall reads a credential from stdin and writes it to a root-owned file
// that the reviewer's group can read. It prints nothing of the credential; an
// empty stdin is a refusal, so a guest that dropped stdin cannot install an
// empty slot. It never replaces an existing slot.
func credInstall(root string, ensureMem func(string) error, args []string, stdin io.Reader, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("cred-install", flag.ContinueOnError)
	fs.SetOutput(stderr)
	dest := fs.String("dest", "", "file to create")
	gid64 := fs.Uint64("gid", 0, "group allowed to read it")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if err := credDest(root, *dest); err != nil {
		fmt.Fprintln(stderr, "cred-install:", err)
		return 2
	}
	if *gid64 < 1 || *gid64 > math.MaxUint32-1 { // 0 is root; 2^32-1 is the kernel's "no change" value

		fmt.Fprintln(stderr, "cred-install: the group must be an unprivileged gid")
		return 2
	}
	gid := int(*gid64)
	b, err := io.ReadAll(io.LimitReader(stdin, model.MaxStdinBytes+1))
	defer func() {
		for i := range b {
			b[i] = 0
		}
	}()
	if err != nil || len(b) == 0 || len(b) > model.MaxStdinBytes {
		fmt.Fprintln(stderr, "cred-install: stdin must hold a credential of 1 to", model.MaxStdinBytes, "bytes")
		return 2
	}
	// the parent must be enterable by the group too: MkdirAll would create a missing parent with the slot directory's own mode
	if err := os.MkdirAll(filepath.Dir(root), 0o755); err != nil { //nolint:gosec // a parent like /run is world-searchable, as it is everywhere
		fmt.Fprintln(stderr, "cred-install: cannot prepare the credential directory")
		return 1
	}
	if err := os.MkdirAll(root, 0o750); err != nil {
		fmt.Fprintln(stderr, "cred-install: cannot prepare the credential directory")
		return 1
	}
	// the credential goes only on memory-backed storage; if the directory is not
	// already on it, put a tmpfs there, and if that cannot be done, install nothing
	if err := ensureMem(root); err != nil {
		fmt.Fprintln(stderr, "cred-install: the credential directory is not memory-backed and could not be made so")
		return 1
	}
	// the group must be able to reach the slot: a directory it cannot enter is a failure, not a quiet default
	if err := os.Chown(root, -1, gid); err != nil {
		fmt.Fprintln(stderr, "cred-install: the credential directory could not be given to the group")
		return 1
	}
	if err := os.Chmod(root, 0o750); err != nil { //nolint:gosec // a directory: owner and the reviewer group may enter it
		fmt.Fprintln(stderr, "cred-install: the credential directory could not be restricted")
		return 1
	}
	f, err := os.OpenFile(*dest, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o440) //nolint:gosec // dest is checked by credDest; 0440 is the point: the reviewer's group reads it
	if err != nil {
		fmt.Fprintln(stderr, "cred-install: the slot cannot be created (it may already exist)")
		return 1
	}
	fail := func() int {
		_ = f.Close()
		_ = os.Remove(*dest)
		fmt.Fprintln(stderr, "cred-install: the slot could not be written")
		return 1
	}
	if err := f.Chmod(0o440); err != nil { //nolint:gosec // see the OpenFile above
		return fail()
	}
	if err := f.Chown(-1, gid); err != nil {
		return fail()
	}
	if _, err := f.Write(b); err != nil {
		return fail()
	}
	if err := f.Close(); err != nil {
		_ = os.Remove(*dest)
		return 1
	}
	return 0
}

// credRemove overwrites a slot with zeros and unlinks it. An absent slot is
// not an error, so a release can run twice.
func credRemove(root string, args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("cred-remove", flag.ContinueOnError)
	fs.SetOutput(stderr)
	dest := fs.String("dest", "", "file to remove")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if err := credDest(root, *dest); err != nil {
		fmt.Fprintln(stderr, "cred-remove:", err)
		return 2
	}
	_ = os.Chmod(*dest, 0o600)                   // the slot is read-only; its owner may open it to overwrite
	f, err := os.OpenFile(*dest, os.O_WRONLY, 0) //nolint:gosec // dest is checked by credDest
	if os.IsNotExist(err) {
		return 0
	}
	if err != nil {
		fmt.Fprintln(stderr, "cred-remove: the slot cannot be opened")
		return 1
	}
	scrubErr := scrub(f)
	if cerr := f.Close(); scrubErr == nil {
		scrubErr = cerr
	}
	// The slot is unlinked even if the overwrite failed: a credential file must
	// not stay behind because the zeroing did not work. The failure is reported.
	if err := os.Remove(*dest); err != nil && !os.IsNotExist(err) {
		fmt.Fprintln(stderr, "cred-remove: the slot cannot be removed")
		return 1
	}
	if scrubErr != nil {
		fmt.Fprintln(stderr, "cred-remove: the slot was removed but could not be overwritten first")
		return 1
	}
	return 0
}

// scrub zeroes the slot's bytes and flushes them. It is a variable so a test
// can force a failure.
var scrub = func(f *os.File) error {
	fi, err := f.Stat()
	if err != nil {
		return err
	}
	if _, err := f.Write(make([]byte, fi.Size())); err != nil {
		return err
	}
	return f.Sync()
}
