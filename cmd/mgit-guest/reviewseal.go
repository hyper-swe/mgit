package main

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
)

// reviewSeal hands the lease to the reviewer's group without handing the
// reviewer ownership of anything:
//
//	mgit-guest review-seal --gid N <lease-dir>
//
// The directory and the files in it become root-owned with group N, the
// directory 0750 and the files 0440. The reviewer can read what it was given,
// and because it owns none of it, it cannot change a mode or an owner back.
// It runs as root, before the reviewer starts. Refs: FEAT-3.347.1, MGIT-333
func reviewSeal(args []string, stderr io.Writer) int {
	return reviewSealWith(args, stderr, os.Chown, ownedByRootWithGroup)
}

// reviewSealWith is reviewSeal with the ownership change and the on-disk owner
// check injected, so the rule that a seal is checked after it is made is testable
// by a user who cannot change owners.
func reviewSealWith(args []string, stderr io.Writer, chown func(path string, uid, gid int) error, owned func(path string, gid int) error) int {
	fs := flag.NewFlagSet("review-seal", flag.ContinueOnError)
	fs.SetOutput(stderr)
	gid := fs.Int("gid", 0, "the reviewer's group id (above zero)")
	evidence := fs.String("evidence-sha256", "", "the sha256 the attestation names for evidence.json")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if *gid <= 0 || fs.NArg() != 1 || !evidenceHex.MatchString(*evidence) {
		fmt.Fprintln(stderr, "review-seal: --gid N (above zero), --evidence-sha256 (64 hex digits) and one lease directory are required")
		return 2
	}
	// What the guest was built from must be what the host checked: a lease
	// changed between that check and this image is refused before anything is
	// handed to the reviewer.
	if err := checkEvidence(fs.Arg(0), *evidence); err != nil {
		fmt.Fprintln(stderr, "review-seal:", err)
		return 1
	}
	if err := sealLease(fs.Arg(0), *gid, chown); err != nil {
		fmt.Fprintln(stderr, "review-seal:", err)
		return 1
	}
	// Look again at what is on disk: a seal that did not take is a failure,
	// never a lease the reviewer happens to own.
	if err := verifySealed(fs.Arg(0), *gid, owned); err != nil {
		fmt.Fprintln(stderr, "review-seal:", err)
		return 1
	}
	return 0
}

// sealLease does the work with the ownership change injected, so the rules are
// testable without root.
func sealLease(dir string, gid int, chown func(path string, uid, gid int) error) error {
	if gid <= 0 {
		return errors.New("the reviewer's group id must be above zero")
	}
	fi, err := os.Lstat(dir)
	if err != nil || !fi.IsDir() || fi.Mode()&os.ModeSymlink != 0 {
		return errors.New("the lease is not a directory")
	}
	ents, err := os.ReadDir(dir)
	if err != nil || len(ents) == 0 || len(ents) > 9 {
		return errors.New("the lease must hold a few regular files")
	}
	var files []string
	for _, e := range ents {
		if isLostAndFound(dir, e) {
			continue
		}
		p := filepath.Join(dir, e.Name())
		fi, err := os.Lstat(p)
		if err != nil || !fi.Mode().IsRegular() {
			return fmt.Errorf("%s is not a regular file", e.Name())
		}
		files = append(files, p)
	}
	for _, p := range files {
		if err := chown(p, 0, gid); err != nil {
			return err
		}
		if err := os.Chmod(p, 0o440); err != nil { //nolint:gosec // group-readable on purpose: the reviewer reads through its group
			return err
		}
	}
	if err := chown(dir, 0, gid); err != nil {
		return err
	}
	return os.Chmod(dir, 0o750) //nolint:gosec // group may enter and read; nobody but root may write
}

// verifySealed checks the owner and the mode of the directory and of every file in it.
func verifySealed(dir string, gid int, owned func(path string, gid int) error) error {
	if err := owned(dir, gid); err != nil {
		return err
	}
	if err := hasMode(dir, 0o750); err != nil {
		return err
	}
	ents, err := os.ReadDir(dir)
	if err != nil {
		return err
	}
	for _, e := range ents {
		if isLostAndFound(dir, e) {
			continue
		}
		p := filepath.Join(dir, e.Name())
		if err := owned(p, gid); err != nil {
			return err
		}
		if err := hasMode(p, 0o440); err != nil {
			return err
		}
	}
	return nil
}

// hasMode fails unless path has exactly the permission bits a seal sets.
func hasMode(path string, want os.FileMode) error {
	fi, err := os.Lstat(path)
	if err != nil {
		return err
	}
	if fi.Mode().Perm() != want {
		return fmt.Errorf("%s has mode %04o, the seal sets %04o", filepath.Base(path), fi.Mode().Perm(), want)
	}
	return nil
}

// lostFoundRootOwned says whether a lost+found is owned by uid 0. It is a
// variable so the rule can be tested by a user who cannot make a root-owned one.
var lostFoundRootOwned = isRootOwned

// isLostAndFound reports whether e is the directory a filesystem keeps at its
// root: named exactly lost+found, a real directory (not a link), empty, and
// mode 0700, owned by root. A volume delivered as an image has one; it is the filesystem's and
// stays root-owned 0700. Anything else by that name is not it.
func isLostAndFound(dir string, e os.DirEntry) bool {
	if e.Name() != "lost+found" {
		return false
	}
	p := filepath.Join(dir, e.Name())
	fi, err := os.Lstat(p)
	if err != nil || fi.Mode()&os.ModeSymlink != 0 || !fi.IsDir() || fi.Mode().Perm() != 0o700 || !lostFoundRootOwned(p) {
		return false
	}
	inside, err := os.ReadDir(p)
	return err == nil && len(inside) == 0
}

var evidenceHex = regexp.MustCompile(`^[0-9a-f]{64}$`)

// checkEvidence fails unless dir/evidence.json is a regular file whose sha256
// is want.
func checkEvidence(dir, want string) error {
	p := filepath.Join(dir, "evidence.json")
	fi, err := os.Lstat(p)
	if err != nil || !fi.Mode().IsRegular() {
		return errors.New("the lease has no evidence.json to check")
	}
	b, err := os.ReadFile(p) //nolint:gosec // a fixed name inside the lease
	if err != nil {
		return errors.New("the evidence cannot be read")
	}
	h := sha256.Sum256(b)
	if hex.EncodeToString(h[:]) != want {
		return errors.New("the evidence is not the one the attestation names")
	}
	return nil
}
