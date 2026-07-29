package guestbase

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// A fake registry keeps these tests offline and deterministic. It speaks the
// subset of the distribution API a public pull uses: a token challenge, a
// manifest (index or single), and content-addressed blobs.

type fakeRegistry struct {
	baseURL   string
	t         *testing.T
	blobs     map[string][]byte // digest -> bytes
	manifests map[string][]byte // tag or digest -> manifest bytes
	mediaType map[string]string // manifest key -> media type
	// requireToken exercises the 401 -> token -> retry flow real registries use.
	requireToken bool
	tokenIssued  bool
}

func newFakeRegistry(t *testing.T) *fakeRegistry {
	return &fakeRegistry{
		t: t, blobs: map[string][]byte{}, manifests: map[string][]byte{},
		mediaType: map[string]string{},
	}
}

// addBlob stores content and returns its digest.
func (f *fakeRegistry) addBlob(content []byte) string {
	sum := sha256.Sum256(content)
	d := "sha256:" + hex.EncodeToString(sum[:])
	f.blobs[d] = content
	return d
}

// addManifest stores a manifest under both its tag and its digest.
func (f *fakeRegistry) addManifest(tag string, mediaType string, doc any) string {
	b, err := json.Marshal(doc)
	if err != nil {
		f.t.Fatal(err)
	}
	sum := sha256.Sum256(b)
	d := "sha256:" + hex.EncodeToString(sum[:])
	f.manifests[d] = b
	f.mediaType[d] = mediaType
	if tag != "" {
		f.manifests[tag] = b
		f.mediaType[tag] = mediaType
	}
	return d
}

func (f *fakeRegistry) start() *httptest.Server {
	mux := http.NewServeMux()
	mux.HandleFunc("/token", func(w http.ResponseWriter, _ *http.Request) {
		f.tokenIssued = true
		_ = json.NewEncoder(w).Encode(map[string]string{"token": "fake-bearer"})
	})
	mux.HandleFunc("/v2/", func(w http.ResponseWriter, r *http.Request) {
		if f.requireToken && r.Header.Get("Authorization") != "Bearer fake-bearer" {
			w.Header().Set("WWW-Authenticate",
				fmt.Sprintf(`Bearer realm="%s/token",service="fake"`, f.baseURL))
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		path := strings.TrimPrefix(r.URL.Path, "/v2/")
		switch {
		case strings.Contains(path, "/manifests/"):
			key := path[strings.LastIndex(path, "/")+1:]
			doc, ok := f.manifests[key]
			if !ok {
				w.WriteHeader(http.StatusNotFound)
				return
			}
			w.Header().Set("Content-Type", f.mediaType[key])
			_, _ = w.Write(doc)
		case strings.Contains(path, "/blobs/"):
			key := path[strings.LastIndex(path, "/")+1:]
			blob, ok := f.blobs[key]
			if !ok {
				w.WriteHeader(http.StatusNotFound)
				return
			}
			_, _ = w.Write(blob)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	})
	srv := httptest.NewServer(mux)
	f.baseURL = srv.URL
	return srv
}

// baseURL is set once the server starts; the token challenge needs it.
func (f *fakeRegistry) setBase(u string) { f.baseURL = u }

// layerTarGz builds a gzipped tar layer from a path->content map. A path
// prefixed with "WH:" becomes an OCI whiteout marker deleting that path.
func layerTarGz(t *testing.T, files map[string]string) []byte {
	t.Helper()
	var buf bytes.Buffer
	zw := gzip.NewWriter(&buf)
	tw := tar.NewWriter(zw)
	for name, content := range files {
		hdr := &tar.Header{Name: name, Mode: 0o644, Size: int64(len(content)), Typeflag: tar.TypeReg}
		if strings.HasSuffix(name, "/") {
			hdr = &tar.Header{Name: name, Mode: 0o755, Typeflag: tar.TypeDir}
		}
		if err := tw.WriteHeader(hdr); err != nil {
			t.Fatal(err)
		}
		if hdr.Typeflag == tar.TypeReg {
			if _, err := tw.Write([]byte(content)); err != nil {
				t.Fatal(err)
			}
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

// singleImage wires a one-layer image and returns the registry + ref.
func singleImage(t *testing.T, files map[string]string) (*fakeRegistry, *httptest.Server, Ref) {
	reg, srv, ref, _ := singleImageWithLayer(t, files)
	return reg, srv, ref
}

// singleImageWithLayer is singleImage plus the layer's digest, for tests that
// need to corrupt exactly the blob the manifest points at.
func singleImageWithLayer(t *testing.T, files map[string]string) (*fakeRegistry, *httptest.Server, Ref, string) {
	t.Helper()
	reg := newFakeRegistry(t)
	srv := reg.start()
	reg.setBase(srv.URL)

	layer := layerTarGz(t, files)
	layerDigest := reg.addBlob(layer)
	configDigest := reg.addBlob([]byte(`{"architecture":"` + ociArch() + `","os":"linux"}`))

	reg.addManifest("v1", mediaTypeOCIManifest, map[string]any{
		"schemaVersion": 2,
		"mediaType":     mediaTypeOCIManifest,
		"config":        map[string]any{"mediaType": "application/vnd.oci.image.config.v1+json", "digest": configDigest, "size": 40},
		"layers": []map[string]any{
			{"mediaType": mediaTypeOCILayerGzip, "digest": layerDigest, "size": len(layer)},
		},
	})
	ref := Ref{Registry: strings.TrimPrefix(srv.URL, "http://"), Repository: "acme/base", Tag: "v1"}
	return reg, srv, ref, layerDigest
}

func TestPull_ExtractsLayersIntoATree(t *testing.T) {
	_, srv, ref := singleImage(t, map[string]string{
		"bin/":           "",
		"bin/sh":         "#!/bin/sh",
		"etc/os-release": "ID=debian",
	})
	defer srv.Close()

	dest := t.TempDir()
	got, err := Pull(context.Background(), ref, dest, PullOptions{PlainHTTP: true})
	if err != nil {
		t.Fatalf("Pull: %v", err)
	}

	for path, want := range map[string]string{
		"bin/sh":         "#!/bin/sh",
		"etc/os-release": "ID=debian",
	} {
		b, err := os.ReadFile(filepath.Join(dest, path)) //nolint:gosec // test-owned temp dir
		if err != nil {
			t.Fatalf("layer file %s missing: %v", path, err)
		}
		if string(b) != want {
			t.Errorf("%s = %q, want %q", path, b, want)
		}
	}
	// The RESOLVED manifest digest is the provenance record; a tag is a
	// moving pointer and is not evidence of anything.
	if !strings.HasPrefix(got.Digest, "sha256:") {
		t.Errorf("pull did not report a resolved digest, got %q", got.Digest)
	}
}

func TestPull_VerifiesEveryBlobDigest(t *testing.T) {
	reg, srv, ref, layerDigest := singleImageWithLayer(t, map[string]string{"bin/sh": "#!/bin/sh"})
	defer srv.Close()

	// Serve DIFFERENT bytes under the digest the manifest names — a registry,
	// or anything between us and it, substituting content. The layer stays a
	// valid gzip/tar so the failure can only come from the digest check.
	reg.blobs[layerDigest] = layerTarGz(t, map[string]string{"bin/sh": "#!/bin/EVIL"})

	_, err := Pull(context.Background(), ref, t.TempDir(), PullOptions{PlainHTTP: true})
	if err == nil {
		t.Fatal("a layer whose bytes do not match its digest must be refused")
	}
	if !strings.Contains(err.Error(), "digest") {
		t.Errorf("error %q does not name the digest mismatch", err)
	}
}

func TestPull_HonoursTheTokenChallenge(t *testing.T) {
	reg, srv, ref := singleImage(t, map[string]string{"bin/sh": "x"})
	defer srv.Close()
	reg.requireToken = true

	if _, err := Pull(context.Background(), ref, t.TempDir(), PullOptions{PlainHTTP: true}); err != nil {
		t.Fatalf("Pull with a token challenge: %v", err)
	}
	if !reg.tokenIssued {
		t.Error("the 401 token challenge was not followed; real registries require it")
	}
}

func TestPull_SelectsTheHostArchFromAnIndex(t *testing.T) {
	reg := newFakeRegistry(t)
	srv := reg.start()
	reg.setBase(srv.URL)
	defer srv.Close()

	// Two per-arch manifests; only the host's must be extracted.
	mk := func(arch, marker string) string {
		layer := layerTarGz(t, map[string]string{"arch": marker})
		ld := reg.addBlob(layer)
		cd := reg.addBlob([]byte(`{"architecture":"` + arch + `","os":"linux"}`))
		return reg.addManifest("", mediaTypeOCIManifest, map[string]any{
			"schemaVersion": 2, "mediaType": mediaTypeOCIManifest,
			"config": map[string]any{"digest": cd, "size": 40},
			"layers": []map[string]any{{"mediaType": mediaTypeOCILayerGzip, "digest": ld, "size": len(layer)}},
		})
	}
	hostArch := ociArch()
	otherArch := "s390x"
	hostManifest := mk(hostArch, "HOST")
	otherManifest := mk(otherArch, "OTHER")

	reg.addManifest("multi", mediaTypeOCIIndex, map[string]any{
		"schemaVersion": 2, "mediaType": mediaTypeOCIIndex,
		"manifests": []map[string]any{
			{"mediaType": mediaTypeOCIManifest, "digest": otherManifest,
				"platform": map[string]string{"os": "linux", "architecture": otherArch}},
			{"mediaType": mediaTypeOCIManifest, "digest": hostManifest,
				"platform": map[string]string{"os": "linux", "architecture": hostArch}},
		},
	})

	dest := t.TempDir()
	ref := Ref{Registry: strings.TrimPrefix(srv.URL, "http://"), Repository: "acme/multi", Tag: "multi"}
	if _, err := Pull(context.Background(), ref, dest, PullOptions{PlainHTTP: true}); err != nil {
		t.Fatalf("Pull: %v", err)
	}
	b, err := os.ReadFile(filepath.Join(dest, "arch")) //nolint:gosec // test-owned temp dir
	if err != nil {
		t.Fatal(err)
	}
	if string(b) != "HOST" {
		t.Errorf("extracted the %s manifest; the guest arch must match the host's (%s)", b, hostArch)
	}
}

func TestPull_RefusesAnIndexWithoutTheHostArch(t *testing.T) {
	reg := newFakeRegistry(t)
	srv := reg.start()
	reg.setBase(srv.URL)
	defer srv.Close()

	reg.addManifest("only-s390x", mediaTypeOCIIndex, map[string]any{
		"schemaVersion": 2, "mediaType": mediaTypeOCIIndex,
		"manifests": []map[string]any{
			{"mediaType": mediaTypeOCIManifest, "digest": "sha256:" + strings.Repeat("b", 64),
				"platform": map[string]string{"os": "linux", "architecture": "s390x"}},
		},
	})

	ref := Ref{Registry: strings.TrimPrefix(srv.URL, "http://"), Repository: "acme/x", Tag: "only-s390x"}
	_, err := Pull(context.Background(), ref, t.TempDir(), PullOptions{PlainHTTP: true})
	if err == nil {
		t.Fatal("an image with no matching architecture must be refused, not half-extracted")
	}
	// Requirement 5: name BOTH arches, so the user knows what they asked for
	// and what they needed.
	for _, want := range []string{ociArch(), "s390x"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q does not name %q", err, want)
		}
	}
}

func TestExtract_RejectsPathEscapes(t *testing.T) {
	// A layer entry escaping the destination would write into the host
	// filesystem during an ordinary `base from`.
	// NOTE: an ABSOLUTE entry like "/etc/passwd" is NOT an escape — image
	// layers commonly use leading slashes and it roots under the base, which
	// is what container runtimes do too. Only traversal escapes.
	tests := []struct{ name, entry string }{
		{name: "parent_traversal", entry: "../escaped"},
		{name: "nested_traversal", entry: "bin/../../escaped"},
		{name: "deep_traversal", entry: "a/b/../../../../escaped"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			reg := newFakeRegistry(t)
			srv := reg.start()
			reg.setBase(srv.URL)
			defer srv.Close()

			layer := layerTarGz(t, map[string]string{tt.entry: "pwned"})
			ld := reg.addBlob(layer)
			cd := reg.addBlob([]byte(`{"architecture":"` + ociArch() + `","os":"linux"}`))
			reg.addManifest("bad", mediaTypeOCIManifest, map[string]any{
				"schemaVersion": 2, "mediaType": mediaTypeOCIManifest,
				"config": map[string]any{"digest": cd, "size": 40},
				"layers": []map[string]any{{"mediaType": mediaTypeOCILayerGzip, "digest": ld, "size": len(layer)}},
			})

			ref := Ref{Registry: strings.TrimPrefix(srv.URL, "http://"), Repository: "acme/bad", Tag: "bad"}
			_, err := Pull(context.Background(), ref, t.TempDir(), PullOptions{PlainHTTP: true})
			if err == nil {
				t.Fatalf("layer entry %q escaped the destination", tt.entry)
			}
			if !strings.Contains(err.Error(), "escape") {
				t.Errorf("error %q does not name the escape", err)
			}
		})
	}
}

func TestExtract_AppliesWhiteouts(t *testing.T) {
	// Layered images delete files from lower layers with .wh. markers. A base
	// that kept deleted files would not be the image the user asked for.
	reg := newFakeRegistry(t)
	srv := reg.start()
	reg.setBase(srv.URL)
	defer srv.Close()

	lower := layerTarGz(t, map[string]string{"bin/old": "gone", "bin/keep": "kept"})
	upper := layerTarGz(t, map[string]string{"bin/.wh.old": ""})
	ld1, ld2 := reg.addBlob(lower), reg.addBlob(upper)
	cd := reg.addBlob([]byte(`{"architecture":"` + ociArch() + `","os":"linux"}`))
	reg.addManifest("wh", mediaTypeOCIManifest, map[string]any{
		"schemaVersion": 2, "mediaType": mediaTypeOCIManifest,
		"config": map[string]any{"digest": cd, "size": 40},
		"layers": []map[string]any{
			{"mediaType": mediaTypeOCILayerGzip, "digest": ld1, "size": len(lower)},
			{"mediaType": mediaTypeOCILayerGzip, "digest": ld2, "size": len(upper)},
		},
	})

	dest := t.TempDir()
	ref := Ref{Registry: strings.TrimPrefix(srv.URL, "http://"), Repository: "acme/wh", Tag: "wh"}
	if _, err := Pull(context.Background(), ref, dest, PullOptions{PlainHTTP: true}); err != nil {
		t.Fatalf("Pull: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dest, "bin", "old")); !os.IsNotExist(err) {
		t.Error("a whited-out file survived; the base is not the image that was requested")
	}
	if _, err := os.Stat(filepath.Join(dest, "bin", "keep")); err != nil {
		t.Errorf("a file the whiteout did not name was removed: %v", err)
	}
}

// ociArch is the image architecture matching this host's guest arch.
func ociArch() string {
	if runtime.GOARCH == "arm64" {
		return "arm64"
	}
	return "amd64"
}

// TestExtract_AbsolutePathsRootUnderTheBase pins the companion behavior: a
// leading slash is normal in image layers and must land inside the base, not
// be refused and not reach the host. Refs: MGIT-61.15
func TestExtract_AbsolutePathsRootUnderTheBase(t *testing.T) {
	_, srv, ref := singleImage(t, map[string]string{"/etc/os-release": "ID=debian"})
	defer srv.Close()

	dest := t.TempDir()
	if _, err := Pull(context.Background(), ref, dest, PullOptions{PlainHTTP: true}); err != nil {
		t.Fatalf("Pull: %v", err)
	}
	b, err := os.ReadFile(filepath.Join(dest, "etc", "os-release")) //nolint:gosec // test-owned temp dir
	if err != nil {
		t.Fatalf("an absolute layer entry did not land under the base: %v", err)
	}
	if string(b) != "ID=debian" {
		t.Errorf("content = %q", b)
	}
}
