package main

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"testing"
)

type chownCall struct {
	path     string
	uid, gid int
}

// asRoot makes the lost+found ownership check read as root-owned, which a test
// run by an ordinary user could not otherwise produce.
func asRoot(t *testing.T) {
	t.Helper()
	was := lostFoundRootOwned
	lostFoundRootOwned = func(string) bool { return true }
	t.Cleanup(func() { lostFoundRootOwned = was })
}

// sealedModes gives a lease the modes the seal sets, so the checks that look at
// what is on disk can be exercised without changing owners.
func sealedModes(t *testing.T, d string) {
	t.Helper()
	ents, err := os.ReadDir(d)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range ents {
		if e.IsDir() {
			continue
		}
		if err := os.Chmod(filepath.Join(d, e.Name()), 0o440); err != nil { //nolint:gosec // the sealed file mode
			t.Fatal(err)
		}
	}
	if err := os.Chmod(d, 0o750); err != nil { //nolint:gosec // the sealed directory mode
		t.Fatal(err)
	}
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
	asRoot(t)
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
	// A file by that name is not the filesystem's directory: it is sealed like
	// any other file, not exempted.
	d2 := lease(t)
	f2 := filepath.Join(d2, "lost+found")
	if err := os.WriteFile(f2, nil, 0o400); err != nil {
		t.Fatal(err)
	}
	var calls2 []chownCall
	if err := sealLease(d2, 7, func(p string, u, g int) error { calls2 = append(calls2, chownCall{p, u, g}); return nil }); err != nil {
		t.Fatal(err)
	}
	sealed := false
	for _, c := range calls2 {
		sealed = sealed || c.path == f2
	}
	if !sealed {
		t.Fatal("a regular file named lost+found was exempted instead of sealed")
	}
}

// The exemption is exact: a directory named lost+found, at the lease root,
// empty, mode 0700, not a link. Anything else with that name is refused.
func TestSealLease_LostAndFoundExemptionIsExact(t *testing.T) {
	asRoot(t)
	noop := func(string, int, int) error { return nil }
	cases := map[string]func(t *testing.T, d string){
		"not empty": func(t *testing.T, d string) {
			lf := filepath.Join(d, "lost+found")
			if err := os.Mkdir(lf, 0o700); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(lf, "x"), nil, 0o400); err != nil {
				t.Fatal(err)
			}
		},
		"a link": func(t *testing.T, d string) {
			other := filepath.Join(t.TempDir(), "o")
			if err := os.Mkdir(other, 0o700); err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink(other, filepath.Join(d, "lost+found")); err != nil {
				t.Fatal(err)
			}
		},
		"open to others": func(t *testing.T, d string) {
			lf := filepath.Join(d, "lost+found")
			if err := os.Mkdir(lf, 0o700); err != nil {
				t.Fatal(err)
			}
			if err := os.Chmod(lf, 0o755); err != nil { //nolint:gosec // the test makes it too open on purpose
				t.Fatal(err)
			}
		},
		"nested, not at the root": func(t *testing.T, d string) {
			if err := os.MkdirAll(filepath.Join(d, "sub", "lost+found"), 0o700); err != nil {
				t.Fatal(err)
			}
		},
		"a different name": func(t *testing.T, d string) {
			if err := os.Mkdir(filepath.Join(d, "Lost+Found"), 0o700); err != nil {
				t.Fatal(err)
			}
		},
	}
	for name, mk := range cases {
		d := lease(t)
		mk(t, d)
		if err := sealLease(d, 7, noop); err == nil {
			t.Fatalf("%s: accepted", name)
		}
	}
	d := lease(t)
	if err := os.Mkdir(filepath.Join(d, "lost+found"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := sealLease(d, 7, noop); err != nil {
		t.Fatalf("the exact lost+found was refused: %v", err)
	}
}

func TestVerifySealed_SkipsOnlyTheFilesystemsLostAndFound(t *testing.T) {
	asRoot(t)
	d := lease(t)
	lf := filepath.Join(d, "lost+found")
	if err := os.Mkdir(lf, 0o700); err != nil {
		t.Fatal(err)
	}
	sealedModes(t, d)
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

func TestReviewSeal_NeedsAGidADigestAndADirectory(t *testing.T) {
	dig := evidenceDigest("x")
	for _, args := range [][]string{{}, {"/x"}, {"--gid", "5"}, {"--gid", "0", "/x"}, {"--gid", "5", "/x"}, {"--gid", "5", "--evidence-sha256", "nothex", "/x"}, {"--gid", "5", "--evidence-sha256", dig}} {
		var e bytes.Buffer
		if code := reviewSeal(args, &e); code != 2 {
			t.Fatalf("%v: exit %d", args, code)
		}
	}
}

func TestVerifySealed_FailsWhenAnythingIsStillOwnedByTheReviewer(t *testing.T) {
	d := lease(t)
	sealedModes(t, d)
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

// The filesystem's lost+found is root's. One owned by anyone else (a directory
// the reviewer's own user made, say) is not exempt: it is refused when sealing
// and checked like any other entry when verifying.
func TestLostAndFound_MustBeRootOwned(t *testing.T) {
	was := lostFoundRootOwned
	lostFoundRootOwned = func(string) bool { return false }
	defer func() { lostFoundRootOwned = was }()
	d := lease(t)
	lf := filepath.Join(d, "lost+found")
	if err := os.Mkdir(lf, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := sealLease(d, 7, func(string, int, int) error { return nil }); err == nil {
		t.Fatal("a lost+found that is not root's was exempted when sealing")
	}
	sealedModes(t, d)
	var seen []string
	_ = verifySealed(d, 7, func(p string, _ int) error { seen = append(seen, p); return nil })
	found := false
	for _, p := range seen {
		found = found || p == lf
	}
	if !found {
		t.Fatal("a lost+found that is not root's was skipped when verifying")
	}
}

// What is on disk must also have the modes the seal sets: a seal that took the
// owner but left a file writable by the group, or the directory open to
// everyone, is not a seal.
func TestVerifySealed_ChecksTheModesToo(t *testing.T) {
	ok := func(string, int) error { return nil }
	d := lease(t) // 0700 directory, 0400 files: not the sealed modes
	if err := verifySealed(d, 5, ok); err == nil {
		t.Fatal("unsealed modes passed")
	}
	if err := sealLease(d, 5, func(string, int, int) error { return nil }); err != nil {
		t.Fatal(err)
	}
	if err := verifySealed(d, 5, ok); err != nil {
		t.Fatalf("sealed modes refused: %v", err)
	}
	ev := filepath.Join(d, "evidence.json")
	for _, bad := range []os.FileMode{0o640, 0o444, 0o660, 0o400} {
		if err := os.Chmod(ev, bad); err != nil { //nolint:gosec // the test sets the mode wrong on purpose
			t.Fatal(err)
		}
		if err := verifySealed(d, 5, ok); err == nil {
			t.Fatalf("file mode %v passed", bad)
		}
	}
	if err := os.Chmod(ev, 0o440); err != nil { //nolint:gosec // back to the sealed mode
		t.Fatal(err)
	}
	for _, bad := range []os.FileMode{0o755, 0o770, 0o700} {
		if err := os.Chmod(d, bad); err != nil { //nolint:gosec // the test sets the mode wrong on purpose
			t.Fatal(err)
		}
		if err := verifySealed(d, 5, ok); err == nil {
			t.Fatalf("directory mode %v passed", bad)
		}
	}
}

// The seal is not trusted because it ran: the verb looks at what is on disk and
// fails if anything did not take, whatever the changes reported.
func TestReviewSeal_FailsWhenTheOnDiskCheckFails(t *testing.T) {
	d := lease(t)
	chown := func(string, int, int) error { return nil }
	var e bytes.Buffer
	notRoot := func(string, int) error { return errors.New("still the reviewer's") }
	if code := reviewSealWith(sealArgs(d, 5), &e, chown, notRoot); code != 1 {
		t.Fatalf("exit %d with a seal that did not take: %s", code, e.String())
	}
	d2 := lease(t)
	var e2 bytes.Buffer
	if code := reviewSealWith(sealArgs(d2, 5), &e2, chown, func(string, int) error { return nil }); code != 0 {
		t.Fatalf("exit %d for a seal that took: %s", code, e2.String())
	}
}

// A lease holds the evidence and the attestation, with room for the filesystem's
// own lost+found; ten entries are not that.
func TestSealLease_TakesNineEntriesAndRefusesTen(t *testing.T) {
	noop := func(string, int, int) error { return nil }
	mk := func(n int) string {
		d := filepath.Join(t.TempDir(), "lease")
		if err := os.Mkdir(d, 0o700); err != nil {
			t.Fatal(err)
		}
		for i := 0; i < n; i++ {
			if err := os.WriteFile(filepath.Join(d, "f"+string(rune('a'+i))), []byte("x"), 0o400); err != nil {
				t.Fatal(err)
			}
		}
		return d
	}
	if err := sealLease(mk(9), 5, noop); err != nil {
		t.Fatalf("nine entries refused: %v", err)
	}
	if err := sealLease(mk(10), 5, noop); err == nil {
		t.Fatal("ten entries accepted")
	}
}

func evidenceDigest(s string) string {
	h := sha256.Sum256([]byte(s))
	return hex.EncodeToString(h[:])
}

// sealArgs is the command line the host gives the verb for a lease whose
// evidence is the "x" that lease() writes.
func sealArgs(dir string, gid int) []string {
	return []string{"--gid", strconv.Itoa(gid), "--evidence-sha256", evidenceDigest("x"), dir}
}

// The evidence the guest was built with must be the evidence the attestation
// names: a lease changed between the host's check and the guest's image is
// refused here, before anything is handed to the reviewer.
func TestReviewSeal_RefusesEvidenceThatIsNotTheAttestedOne(t *testing.T) {
	var calls int
	chown := func(string, int, int) error { calls++; return nil }
	ok := func(string, int) error { return nil }

	d := lease(t)
	var e bytes.Buffer
	args := []string{"--gid", "5", "--evidence-sha256", evidenceDigest("something else"), d}
	if code := reviewSealWith(args, &e, chown, ok); code != 1 || calls != 0 {
		t.Fatalf("a different digest: exit %d, %d chowns: %s", code, calls, e.String())
	}
	d2 := lease(t)
	ev2 := filepath.Join(d2, "evidence.json")
	if err := os.Chmod(ev2, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(ev2, []byte("tampered"), 0o600); err != nil {
		t.Fatal(err)
	}
	if code := reviewSealWith(sealArgs(d2, 5), &e, chown, ok); code != 1 || calls != 0 {
		t.Fatalf("changed evidence: exit %d, %d chowns", code, calls)
	}
	d3 := lease(t)
	if err := os.Remove(filepath.Join(d3, "evidence.json")); err != nil {
		t.Fatal(err)
	}
	if code := reviewSealWith(sealArgs(d3, 5), &e, chown, ok); code != 1 || calls != 0 {
		t.Fatalf("no evidence: exit %d, %d chowns", code, calls)
	}
	d4 := lease(t)
	if code := reviewSealWith(sealArgs(d4, 5), &e, chown, ok); code != 0 || calls == 0 {
		t.Fatalf("the attested evidence was refused: exit %d: %s", code, e.String())
	}
}
