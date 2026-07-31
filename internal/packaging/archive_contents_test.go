// Package packaging's archive tests read the ACTUAL release tarballs.
//
// The distinction is the whole reason this file exists. A previous check
// reported "the darwin/arm64 archive carries ELF 64-bit LSB, ARM aarch64" —
// but it had inspected `dist/mgit-guest_linux_arm64*/`, a goreleaser BUILD
// directory, not the tarball a user downloads. Build directories always
// contain the binaries; that says nothing about whether they were packaged.
// Everything here goes through the archive reader, so the thing under test is
// the artifact that ships. Refs: MGIT-65, MGIT-61.15
package packaging

import (
	"archive/tar"
	"archive/zip"
	"compress/gzip"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// guestBinariesInArchive are the paths `mgit sandbox base` looks for beside
// the host binary. They are the entire delivery mechanism for the guest
// userspace: without them an installed mgit can compose no base at all, and
// the OCI story does not work on a machine with no Go toolchain.
var guestBinariesInArchive = []string{"guest/mgit", "guest/mgit-guest"}

// TestArchives_ShipTheGuestBinaries is the assertion that would have caught
// MGIT-65's second defect.
func TestArchives_ShipTheGuestBinaries(t *testing.T) {
	archives := builtArchives(t)
	for _, archive := range archives {
		t.Run(filepath.Base(archive), func(t *testing.T) {
			names, err := archiveEntries(archive)
			if err != nil {
				t.Fatalf("read archive: %v", err)
			}
			for _, want := range guestBinariesInArchive {
				if !contains(names, want) {
					t.Errorf("%s is missing from the archive; an installed mgit cannot "+
						"compose a guest base without it.\ncontents:\n  %s",
						want, strings.Join(names, "\n  "))
				}
			}
		})
	}
}

// TestArchives_ShipTheHostBinariesTheyClaimTo guards the other half of the
// layout: mgit everywhere, and mgit-sandboxd wherever a sandbox backend
// exists. The brew formula shipped without mgit-sandboxd once (MGIT-44);
// this is the archive-side equivalent.
func TestArchives_ShipTheHostBinariesTheyClaimTo(t *testing.T) {
	for _, archive := range builtArchives(t) {
		base := filepath.Base(archive)
		t.Run(base, func(t *testing.T) {
			names, err := archiveEntries(archive)
			if err != nil {
				t.Fatalf("read archive: %v", err)
			}
			host := "mgit"
			if strings.Contains(base, "windows") {
				host = "mgit.exe"
			}
			if !contains(names, host) {
				t.Errorf("%s is missing the %s binary", base, host)
			}
			// The sandbox backends: linux (firecracker) and darwin/arm64
			// (libkrun). Nowhere else has one, by design.
			wantDaemon := strings.Contains(base, "linux") || strings.Contains(base, "darwin_arm64")
			if got := contains(names, "mgit-sandboxd"); got != wantDaemon {
				t.Errorf("%s: mgit-sandboxd present = %v, want %v", base, got, wantDaemon)
			}
		})
	}
}

// TestArchives_ShipGuestBinariesForTheirOwnArchitecture checks the bytes, not
// just the path.
//
// A guest binary of the wrong architecture is worse than a missing one: it
// installs cleanly, pins into images.lock, and fails at boot. libkrun uses
// hardware virtualization — there is no emulation to cross architectures with
// — so each archive carries exactly its own, which is how linux/arm64 and
// linux/amd64 are both covered across the set. Refs: MGIT-65, MGIT-61.15
func TestArchives_ShipGuestBinariesForTheirOwnArchitecture(t *testing.T) {
	// ELF e_machine values, little-endian at offset 18.
	const elfX8664, elfAArch64 = 0x3E, 0xB7
	for _, archive := range builtArchives(t) {
		base := filepath.Base(archive)
		t.Run(base, func(t *testing.T) {
			want := elfAArch64
			if strings.Contains(base, "amd64") {
				want = elfX8664
			}
			for _, name := range guestBinariesInArchive {
				head, err := archiveFileHead(archive, name, 20)
				if err != nil {
					t.Fatalf("read %s: %v", name, err)
				}
				if string(head[:4]) != "\x7fELF" {
					t.Fatalf("%s is not an ELF binary; the guest runs Linux", name)
				}
				if got := int(head[18]) | int(head[19])<<8; got != want {
					t.Errorf("%s has e_machine 0x%02X, want 0x%02X for %s — a wrong-architecture "+
						"guest binary installs fine and only fails at boot", name, got, want, base)
				}
			}
		})
	}
}

// TestArchives_ShipTheLicense pins what declaring `files` in goreleaser costs
// if you forget: the default file set (LICENSE, README, CHANGELOG) is
// REPLACED, not extended. Adding the guest binaries silently dropped the
// license text from every archive of an Apache-2.0 product — caught only by
// building an archive with the guest packaging removed and reading what came
// back. Refs: MGIT-65
func TestArchives_ShipTheLicense(t *testing.T) {
	for _, archive := range builtArchives(t) {
		t.Run(filepath.Base(archive), func(t *testing.T) {
			names, err := archiveEntries(archive)
			if err != nil {
				t.Fatalf("read archive: %v", err)
			}
			for _, want := range []string{"LICENSE", "README.md"} {
				if !contains(names, want) {
					t.Errorf("%s is missing from the archive:\n  %s", want, strings.Join(names, "\n  "))
				}
			}
		})
	}
}

// builtArchives returns the release archives, skipping LOUDLY when none have
// been built — this test asserts against a real artifact, so there is nothing
// honest to say without one.
func builtArchives(t *testing.T) []string {
	t.Helper()
	dist := filepath.Join(repoRoot(t), "dist")
	var found []string
	for _, pattern := range []string{"*.tar.gz", "*.zip"} {
		matches, err := filepath.Glob(filepath.Join(dist, pattern))
		if err != nil {
			t.Fatal(err)
		}
		found = append(found, matches...)
	}
	if len(found) == 0 {
		t.Skipf("SKIP (archive contents): no release archives in %s. Build them with "+
			"`make verify-archive`, which runs goreleaser and then this test.", dist)
	}
	return found
}

// archiveFileHead returns the first n bytes of one file inside an archive.
func archiveFileHead(archive, name string, n int) ([]byte, error) {
	if strings.HasSuffix(archive, ".zip") {
		return zipFileHead(archive, name, n)
	}
	return tarGzFileHead(archive, name, n)
}

func tarGzFileHead(archive, name string, n int) ([]byte, error) {
	f, err := os.Open(archive) //nolint:gosec // a path this test globbed from dist/
	if err != nil {
		return nil, err
	}
	defer func() { _ = f.Close() }()
	zr, err := gzip.NewReader(f)
	if err != nil {
		return nil, err
	}
	defer func() { _ = zr.Close() }()

	tr := tar.NewReader(zr)
	for {
		hdr, err := tr.Next()
		if errors.Is(err, io.EOF) {
			return nil, fmt.Errorf("%s not found in %s", name, archive)
		}
		if err != nil {
			return nil, err
		}
		if hdr.Name != name {
			continue
		}
		head := make([]byte, n)
		if _, err := io.ReadFull(tr, head); err != nil {
			return nil, err
		}
		return head, nil
	}
}

func zipFileHead(archive, name string, n int) ([]byte, error) {
	r, err := zip.OpenReader(archive)
	if err != nil {
		return nil, err
	}
	defer func() { _ = r.Close() }()
	for _, f := range r.File {
		if f.Name != name {
			continue
		}
		return readHead(f, n)
	}
	return nil, fmt.Errorf("%s not found in %s", name, archive)
}

// readHead reads the first n bytes of one zip entry, closing it before
// returning so the reader is not held open across the caller's loop.
func readHead(f *zip.File, n int) ([]byte, error) {
	rc, err := f.Open()
	if err != nil {
		return nil, err
	}
	defer func() { _ = rc.Close() }()
	head := make([]byte, n)
	if _, err := io.ReadFull(rc, head); err != nil {
		return nil, err
	}
	return head, nil
}

// archiveEntries lists the file paths inside a .tar.gz or .zip.
func archiveEntries(path string) ([]string, error) {
	if strings.HasSuffix(path, ".zip") {
		return zipEntries(path)
	}
	return tarGzEntries(path)
}

func tarGzEntries(path string) ([]string, error) {
	f, err := os.Open(path) //nolint:gosec // a path this test globbed from dist/
	if err != nil {
		return nil, err
	}
	defer func() { _ = f.Close() }()
	zr, err := gzip.NewReader(f)
	if err != nil {
		return nil, err
	}
	defer func() { _ = zr.Close() }()

	var names []string
	tr := tar.NewReader(zr)
	for {
		hdr, err := tr.Next()
		if errors.Is(err, io.EOF) {
			return names, nil
		}
		if err != nil {
			return nil, err
		}
		names = append(names, hdr.Name)
	}
}

func zipEntries(path string) ([]string, error) {
	r, err := zip.OpenReader(path)
	if err != nil {
		return nil, err
	}
	defer func() { _ = r.Close() }()
	names := make([]string, 0, len(r.File))
	for _, f := range r.File {
		names = append(names, f.Name)
	}
	return names, nil
}

func contains(haystack []string, needle string) bool {
	for _, s := range haystack {
		if s == needle {
			return true
		}
	}
	return false
}
