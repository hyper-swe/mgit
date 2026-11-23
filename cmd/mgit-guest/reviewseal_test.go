package main

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"testing"
)

type chownCall struct {
	path     string
	uid, gid int
}

func lease(t *testing.T) string {
	t.Helper()
	d := filepath.Join(t.TempDir(), "lease")
	if err := os.Mkdir(d, 0o700); err != nil {
		t.Fatal(err)
	}
	for _, n := range []string{"evidence.json", "attestation.json"} {
		if err := os.WriteFile(filepath.Join(d, n), []byte("x"), 0o400); err != nil {
			t.Fatal(err)
		}
	}
	return d
}

func TestSealLease_MakesItRootOwnedAndReadableToTheReviewerGroupOnly(t *testing.T) {
	d := lease(t)
	var calls []chownCall
	err := sealLease(d, 4242, func(p string, u, g int) error { calls = append(calls, chownCall{p, u, g}); return nil })
	if err != nil {
		t.Fatal(err)
	}
	if fi, _ := os.Stat(d); fi.Mode().Perm() != 0o750 {
		t.Fatalf("dir mode %v", fi.Mode().Perm())
	}
	for _, n := range []string{"evidence.json", "attestation.json"} {
		if fi, _ := os.Stat(filepath.Join(d, n)); fi.Mode().Perm() != 0o440 {
			t.Fatalf("%s mode %v", n, fi.Mode().Perm())
		}
	}
	seen := map[string]bool{}
	for _, c := range calls {
		if c.uid != 0 || c.gid != 4242 {
			t.Fatalf("owner %+v: the reviewer's group may read, nothing is handed to the reviewer", c)
		}
		seen[c.path] = true
	}
	if len(calls) != 3 || !seen[d] || !seen[filepath.Join(d, "evidence.json")] || !seen[filepath.Join(d, "attestation.json")] {
		t.Fatalf("calls %+v", calls)
	}
}

// A volume delivered as a filesystem image carries its own lost+found
// directory. It is the filesystem's, root-owned 0700, and not the reviewer's to
// read, so it is left alone rather than refused or handed over.
func TestSealLease_LeavesAFilesystemsLostAndFoundAlone(t *testing.T) {
	d := lease(t)
	lf := filepath.Join(d, "lost+found")
	if err := os.Mkdir(lf, 0o700); err != nil {
		t.Fatal(err)
	}
	var calls []chownCall
	if err := sealLease(d, 7, func(p string, u, g int) error { calls = append(calls, chownCall{p, u, g}); return nil }); err != nil {
		t.Fatal(err)
	}
	for _, c := range calls {
		if c.path == lf {
			t.Fatal("lost+found was handed to the reviewer's group")
		}
	}
	if fi, _ := os.Stat(lf); fi.Mode().Perm() != 0o700 {
		t.Fatalf("lost+found mode %v", fi.Mode().Perm())
	}
	// A file by that name is not the filesystem's directory.
	d2 := lease(t)
	if err := os.WriteFile(filepath.Join(d2, "lost+found"), nil, 0o400); err != nil {
		t.Fatal(err)
	}
	if err := sealLease(d2, 7, func(string, int, int) error { return nil }); err == nil {
		t.Fatal("a regular file named lost+found was accepted as the filesystem's directory")
	}
}

func TestVerifySealed_SkipsOnlyTheFilesystemsLostAndFound(t *testing.T) {
	d := lease(t)
	lf := filepath.Join(d, "lost+found")
	if err := os.Mkdir(lf, 0o700); err != nil {
		t.Fatal(err)
	}
	var seen []string
	if err := verifySealed(d, 5, func(p string, _ int) error { seen = append(seen, p); return nil }); err != nil {
		t.Fatal(err)
	}
	for _, p := range seen {
		if p == lf {
			t.Fatal("lost+found was checked as the reviewer's")
		}
	}
	if len(seen) != 3 {
		t.Fatalf("checked %v", seen)
	}
}

func TestSealLease_RefusesWhatIsNotTheExpectedShape(t *testing.T) {
	noop := func(string, int, int) error { return nil }
	cases := map[string]func(t *testing.T) (string, int){
		"gid zero":     func(t *testing.T) (string, int) { return lease(t), 0 },
		"gid negative": func(t *testing.T) (string, int) { return lease(t), -3 },
		"missing dir":  func(t *testing.T) (string, int) { return filepath.Join(t.TempDir(), "none"), 5 },
		"dir is link": func(t *testing.T) (string, int) {
			d := lease(t)
			l := filepath.Join(filepath.Dir(d), "l")
			if err := os.Symlink(d, l); err != nil {
				t.Fatal(err)
			}
			return l, 5
		},
		"nested dir": func(t *testing.T) (string, int) {
			d := lease(t)
			if err := os.Mkdir(filepath.Join(d, "sub"), 0o700); err != nil {
				t.Fatal(err)
			}
			return d, 5
		},
		"entry is link": func(t *testing.T) (string, int) {
			d := lease(t)
			if err := os.Symlink("/etc/passwd", filepath.Join(d, "p")); err != nil {
				t.Fatal(err)
			}
			return d, 5
		},
		"empty": func(t *testing.T) (string, int) {
			d := filepath.Join(t.TempDir(), "e")
			if err := os.Mkdir(d, 0o700); err != nil {
				t.Fatal(err)
			}
			return d, 5
		},
	}
	for name, mk := range cases {
		d, gid := mk(t)
		if err := sealLease(d, gid, noop); err == nil {
			t.Fatalf("%s: accepted", name)
		}
	}
}

func TestSealLease_AFailedChownIsAFailure(t *testing.T) {
	d := lease(t)
	if err := sealLease(d, 5, func(string, int, int) error { return errors.New("denied") }); err == nil {
		t.Fatal("a chown failure was swallowed")
	}
}

func TestReviewSeal_NeedsAGidAndADirectory(t *testing.T) {
	for _, args := range [][]string{{}, {"/x"}, {"--gid", "5"}, {"--gid", "0", "/x"}} {
		var e bytes.Buffer
		if code := reviewSeal(args, &e); code != 2 {
			t.Fatalf("%v: exit %d", args, code)
		}
	}
}

func TestVerifySealed_FailsWhenAnythingIsStillOwnedByTheReviewer(t *testing.T) {
	d := lease(t)
	bad := filepath.Join(d, "evidence.json")
	owned := func(p string, _ int) error {
		if p == bad {
			return errors.New("owned by the reviewer")
		}
		return nil
	}
	if err := verifySealed(d, 5, owned); err == nil {
		t.Fatal("a file the reviewer owns passed")
	}
	if err := verifySealed(d, 5, func(string, int) error { return nil }); err != nil {
		t.Fatal(err)
	}
	if err := verifySealed(d, 5, func(p string, _ int) error {
		if p == d {
			return errors.New("dir")
		}
		return nil
	}); err == nil {
		t.Fatal("a directory the reviewer owns passed")
	}
}
