package guestbase

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// LAYER STACKING IS WHERE AN EXTRACTOR IS ACTUALLY WRONG OR RIGHT.
//
// A one-layer image (debian:12) exercises almost none of the format. The
// images people will really use — node:22, python:3.12 — are five to fifteen
// layers that delete, replace and re-link each other's files, and getting any
// of it wrong produces a base that looks fine and behaves strangely: a tool
// that should have been removed still on PATH, a symlink pointing at a file
// from a layer that deleted it, a binary that lost its executable bit.
// Refs: MGIT-61.15, FR-17.17

// layeredImage wires a multi-layer image; layers are applied in order.
func layeredImage(t *testing.T, layers ...*bytes.Buffer) (*httptest.Server, Ref) {
	t.Helper()
	reg := newFakeRegistry(t)
	srv := reg.start()
	reg.setBase(srv.URL)

	descriptors := make([]map[string]any, 0, len(layers))
	for _, layer := range layers {
		digest := reg.addBlob(layer.Bytes())
		descriptors = append(descriptors, map[string]any{
			"mediaType": mediaTypeOCILayerGzip, "digest": digest, "size": layer.Len(),
		})
	}
	configDigest := reg.addBlob([]byte(`{"architecture":"` + ociArch() + `","os":"linux"}`))
	reg.addManifest("v1", mediaTypeOCIManifest, map[string]any{
		"schemaVersion": 2,
		"mediaType":     mediaTypeOCIManifest,
		"config": map[string]any{
			"mediaType": "application/vnd.oci.image.config.v1+json",
			"digest":    configDigest, "size": 40,
		},
		"layers": descriptors,
	})
	return srv, Ref{
		Registry: strings.TrimPrefix(srv.URL, "http://"), Repository: "acme/base", Tag: "v1",
	}
}

// tarLayer builds one gzipped layer from explicit tar headers, so a test can
// write symlinks, hard links, whiteouts and modes that layerTarGz cannot.
func tarLayer(t *testing.T, write func(tw *tar.Writer)) *bytes.Buffer {
	t.Helper()
	var buf bytes.Buffer
	zw := gzip.NewWriter(&buf)
	tw := tar.NewWriter(zw)
	write(tw)
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	return &buf
}

// file writes one regular tar entry.
func file(t *testing.T, tw *tar.Writer, name, content string, mode int64) {
	t.Helper()
	if err := tw.WriteHeader(&tar.Header{
		Name: name, Mode: mode, Size: int64(len(content)), Typeflag: tar.TypeReg,
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := tw.Write([]byte(content)); err != nil {
		t.Fatal(err)
	}
}

func TestPull_LaterLayersDeleteAndReplaceEarlierOnes(t *testing.T) {
	lower := tarLayer(t, func(tw *tar.Writer) {
		file(t, tw, "usr/bin/keep", "keep", 0o644)
		file(t, tw, "usr/bin/gone", "gone", 0o644)
		file(t, tw, "etc/conf", "old", 0o644)
	})
	upper := tarLayer(t, func(tw *tar.Writer) {
		// The OCI whiteout convention: an empty .wh.<name> marker deletes
		// <name> from every layer below.
		file(t, tw, "usr/bin/.wh.gone", "", 0o644)
		file(t, tw, "etc/conf", "new", 0o644)
	})
	srv, ref := layeredImage(t, lower, upper)
	defer srv.Close()

	dest := t.TempDir()
	if _, err := Pull(context.Background(), ref, dest, PullOptions{PlainHTTP: true}); err != nil {
		t.Fatalf("Pull: %v", err)
	}

	if _, err := os.Lstat(filepath.Join(dest, "usr", "bin", "gone")); !os.IsNotExist(err) {
		t.Error("a whited-out file survived; the base carries a tool the image deleted")
	}
	if got := readFile(t, dest, "etc/conf"); got != "new" {
		t.Errorf("etc/conf = %q, want the upper layer's %q", got, "new")
	}
	if got := readFile(t, dest, "usr/bin/keep"); got != "keep" {
		t.Errorf("usr/bin/keep = %q; an untouched file must survive", got)
	}
}

func TestPull_OpaqueWhiteoutClearsTheDirectory(t *testing.T) {
	lower := tarLayer(t, func(tw *tar.Writer) {
		file(t, tw, "opt/tool/a", "a", 0o644)
		file(t, tw, "opt/tool/b", "b", 0o644)
		file(t, tw, "opt/other", "other", 0o644)
	})
	upper := tarLayer(t, func(tw *tar.Writer) {
		// .wh..wh..opq hides EVERYTHING below in this directory — the marker
		// a rebuild step leaves when it replaces a whole tree.
		file(t, tw, "opt/tool/"+opaqueWhiteout, "", 0o644)
		file(t, tw, "opt/tool/c", "c", 0o644)
	})
	srv, ref := layeredImage(t, lower, upper)
	defer srv.Close()

	dest := t.TempDir()
	if _, err := Pull(context.Background(), ref, dest, PullOptions{PlainHTTP: true}); err != nil {
		t.Fatalf("Pull: %v", err)
	}

	for _, gone := range []string{"opt/tool/a", "opt/tool/b"} {
		if _, err := os.Lstat(filepath.Join(dest, filepath.FromSlash(gone))); !os.IsNotExist(err) {
			t.Errorf("%s survived an opaque whiteout", gone)
		}
	}
	if got := readFile(t, dest, "opt/tool/c"); got != "c" {
		t.Errorf("opt/tool/c = %q; the upper layer's own files must remain", got)
	}
	if got := readFile(t, dest, "opt/other"); got != "other" {
		t.Errorf("opt/other = %q; an opaque marker must not escape its directory", got)
	}
}

func TestPull_PreservesLinksAndTheExecutableBit(t *testing.T) {
	layer := tarLayer(t, func(tw *tar.Writer) {
		file(t, tw, "bin/busybox", "ELF", 0o755)
		file(t, tw, "etc/data", "data", 0o644)
		if err := tw.WriteHeader(&tar.Header{
			// Absolute target: normal in a userspace tree, and it resolves
			// against the GUEST's root, not the host's.
			Name: "bin/sh", Typeflag: tar.TypeSymlink, Linkname: "/bin/busybox", Mode: 0o777,
		}); err != nil {
			t.Fatal(err)
		}
		if err := tw.WriteHeader(&tar.Header{
			Name: "bin/ash", Typeflag: tar.TypeLink, Linkname: "bin/busybox", Mode: 0o755,
		}); err != nil {
			t.Fatal(err)
		}
	})
	srv, ref := layeredImage(t, layer)
	defer srv.Close()

	dest := t.TempDir()
	if _, err := Pull(context.Background(), ref, dest, PullOptions{PlainHTTP: true}); err != nil {
		t.Fatalf("Pull: %v", err)
	}

	target, err := os.Readlink(filepath.Join(dest, "bin", "sh"))
	if err != nil {
		t.Fatalf("bin/sh must be a symlink: %v", err)
	}
	if target != "/bin/busybox" {
		t.Errorf("bin/sh -> %q, want the target verbatim; rewriting it would break it in the guest", target)
	}

	if got := readFile(t, dest, "bin/ash"); got != "ELF" {
		t.Errorf("the hard link's content = %q, want the file it points at", got)
	}

	exe, err := os.Stat(filepath.Join(dest, "bin", "busybox"))
	if err != nil {
		t.Fatal(err)
	}
	if exe.Mode()&0o100 == 0 {
		t.Error("bin/busybox lost its executable bit; nothing in the guest could run it")
	}
	data, err := os.Stat(filepath.Join(dest, "etc", "data"))
	if err != nil {
		t.Fatal(err)
	}
	if data.Mode()&0o111 != 0 {
		t.Error("etc/data gained an executable bit it never had")
	}
}

// readFile reads a slash-separated path under root.
func readFile(t *testing.T, root, rel string) string {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(rel))) //nolint:gosec // test temp path
	if err != nil {
		t.Fatalf("read %s: %v", rel, err)
	}
	return string(data)
}
