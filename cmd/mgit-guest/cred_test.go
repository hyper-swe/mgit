package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/hyper-swe/mgit/internal/model"
)

const fakeCred = "FAKE-KEY-0123456789-not-real"

func credArgs(root, name string) []string {
	return []string{"--dest", filepath.Join(root, name), "--gid", itoa(os.Getgid())}
}

func itoa(n int) string { return fmtInt(n) }

func fmtInt(n int) string {
	if n == 0 {
		return "0"
	}
	var b []byte
	for ; n > 0; n /= 10 {
		b = append([]byte{byte('0' + n%10)}, b...)
	}
	return string(b)
}

func TestCredInstallWritesAReadOnlyFileAndSaysNothingOfIt(t *testing.T) {
	root := filepath.Join(t.TempDir(), "cred")
	var out, errb bytes.Buffer
	if code := credInstall(root, credArgs(root, "slot1"), strings.NewReader(fakeCred), &out, &errb); code != 0 {
		t.Fatalf("exit %d: %s", code, errb.String())
	}
	if strings.Contains(out.String()+errb.String(), fakeCred) {
		t.Fatal("the secret was printed")
	}
	p := filepath.Join(root, "slot1")
	fi, err := os.Stat(p)
	if err != nil || fi.Mode().Perm() != 0o440 {
		t.Fatalf("mode %v err %v", fi, err)
	}
	if b, _ := os.ReadFile(p); string(b) != fakeCred { //nolint:gosec // test path
		t.Fatalf("content %q", b)
	}
}

func TestCredInstallRefusesAnEmptyStdinAnOversizedOneAndAFileThatExists(t *testing.T) {
	root := filepath.Join(t.TempDir(), "cred")
	var o, e bytes.Buffer
	if code := credInstall(root, credArgs(root, "a"), strings.NewReader(""), &o, &e); code == 0 {
		t.Error("an empty stdin was accepted (an older guest drops stdin)")
	}
	big := strings.NewReader(strings.Repeat("x", model.MaxStdinBytes+1))
	if code := credInstall(root, credArgs(root, "b"), big, &o, &e); code == 0 {
		t.Error("an oversized stdin was accepted")
	}
	if code := credInstall(root, credArgs(root, "c"), strings.NewReader("one"), &o, &e); code != 0 {
		t.Fatal("first install failed")
	}
	// even when the file could be opened for writing (root in a guest can), an installed slot is not replaced
	if err := os.Chmod(filepath.Join(root, "c"), 0o600); err != nil {
		t.Fatal(err)
	}
	if code := credInstall(root, credArgs(root, "c"), strings.NewReader("two"), &o, &e); code == 0 {
		t.Error("an installed slot was overwritten")
	}
	if strings.Contains(o.String()+e.String(), "one") && strings.Contains(o.String()+e.String(), "two") {
		t.Error("output echoed the secrets")
	}
}

func TestCredInstallRefusesADestOutsideTheCredentialDirectory(t *testing.T) {
	root := filepath.Join(t.TempDir(), "cred")
	for _, dest := range []string{
		filepath.Join(root, "..", "x"), filepath.Join(root, "sub", "x"), "relative", root, filepath.Join(root, ".hidden"),
		filepath.Join(root, "a b"),
	} {
		var o, e bytes.Buffer
		args := []string{"--dest", dest, "--gid", fmtInt(os.Getgid())}
		if code := credInstall(root, args, strings.NewReader(fakeCred), &o, &e); code == 0 {
			t.Errorf("%q was accepted", dest)
		}
	}
}

func TestCredInstallRefusesGidZero(t *testing.T) {
	root := filepath.Join(t.TempDir(), "cred")
	var o, e bytes.Buffer
	if code := credInstall(root, []string{"--dest", filepath.Join(root, "s"), "--gid", "0"}, strings.NewReader(fakeCred), &o, &e); code != 2 {
		t.Errorf("group zero must be refused up front (exit 2), got %d", code)
	}
}

func TestCredRemoveOverwritesThenUnlinks(t *testing.T) {
	root := filepath.Join(t.TempDir(), "cred")
	var o, e bytes.Buffer
	if code := credInstall(root, credArgs(root, "s"), strings.NewReader(fakeCred), &o, &e); code != 0 {
		t.Fatal(e.String())
	}
	p := filepath.Join(root, "s")
	f, err := os.Open(p) //nolint:gosec // test path
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if code := credRemove(root, []string{"--dest", p}, &o, &e); code != 0 {
		t.Fatal(e.String())
	}
	if _, err := os.Stat(p); err == nil {
		t.Fatal("the file is still there")
	}
	// the open descriptor, held across the removal, now reads zeros: it was overwritten, not just unlinked
	b := make([]byte, len(fakeCred))
	n, _ := f.ReadAt(b, 0)
	if strings.Contains(string(b[:n]), "FAKE") {
		t.Fatal("the bytes survived on the inode")
	}
	if code := credRemove(root, []string{"--dest", p}, &o, &e); code != 0 {
		t.Error("removing an absent slot must be a no-op so release can run twice")
	}
}

func TestCredInstallFailsWhenTheDirectoryCannotBeGivenToTheGroup(t *testing.T) {
	root := filepath.Join(t.TempDir(), "cred")
	var o, e bytes.Buffer
	// a group this process is not a member of: a non-root chown is refused, and the install must say so
	gid := os.Getgid() + 7919
	if os.Getuid() == 0 {
		t.Skip("root can give a directory to any group")
	}
	args := []string{"--dest", filepath.Join(root, "s"), "--gid", fmtInt(gid)}
	if code := credInstall(root, args, strings.NewReader(fakeCred), &o, &e); code == 0 {
		t.Fatal("the install succeeded with a directory the group cannot reach")
	}
	if _, err := os.Stat(filepath.Join(root, "s")); err == nil {
		t.Fatal("a slot was left behind")
	}
}
