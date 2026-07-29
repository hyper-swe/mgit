package main

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// `mgit sandbox base set` is the bring-your-own-tree path (MGIT-61.15): point
// mgit at a Linux userspace directory and it becomes the read-only guest base
// every sandbox for this repo boots from. It is the GA-scoped half of that
// ticket; `base from <oci-ref>` is the fast follow.
//
// The base is a DIRECTORY because libkrun shares the guest root over
// virtio-fs and libkrunfw supplies the kernel — there is no rootfs image and
// no kernel to register. Refs: MGIT-61.15, ADR-010

// userspaceTree builds a plausible Linux userspace: the mount points
// mgit-guest needs at boot, plus a stand-in binary.
func userspaceTree(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	for _, d := range []string{"bin", "sbin", "proc", "dev", "tmp", "mnt", "usr/lib"} {
		require.NoError(t, os.MkdirAll(filepath.Join(root, d), 0o750))
	}
	//nolint:gosec // G306: a shell in a userspace tree must be executable
	require.NoError(t, os.WriteFile(filepath.Join(root, "bin", "sh"), []byte("#!/bin/sh\n"), 0o700))
	return root
}

func TestSandboxBaseSet_PinsSignsAndInjectsTheSupervisor(t *testing.T) {
	repo := newRepo(t)
	tree := userspaceTree(t)

	out, err := initTrustRoot(t, repo)
	require.NoError(t, err, "trust root: %s", out)

	out, err = runBase(t, repo, "set", tree, "--guest-bin-dir", fakeGuestBins(t))
	require.NoError(t, err, "base set: %s", out)

	// A digest-pinned reference, the same shape image add prints.
	assert.Contains(t, out, "sha256:", "the base must be pinned by digest, got %q", out)

	// Requirement 4 of MGIT-61.15: mgit-guest is injected by US and runs as
	// PID 1 — a base image's own entrypoint must never displace it.
	guest := filepath.Join(tree, "sbin", "mgit-guest")
	info, err := os.Stat(guest)
	require.NoError(t, err, "mgit-guest must be injected into the base tree")
	assert.NotZero(t, info.Mode()&0o111, "the injected supervisor must be executable")

	// And the CLI, so an agent can run the checkpoint loop in the sandbox.
	_, err = os.Stat(filepath.Join(tree, "bin", "mgit"))
	require.NoError(t, err, "the mgit CLI must be injected into the base tree")
}

func TestSandboxBaseSet_RefusesATreeThatCannotBoot(t *testing.T) {
	tests := []struct {
		name    string
		build   func(t *testing.T) string
		wantErr string
	}{
		{
			name:    "not_a_directory",
			build:   func(t *testing.T) string { return filepath.Join(t.TempDir(), "nope") },
			wantErr: "base",
		},
		{
			// The failure a real BYO tree hits first: mgit-guest mounts into
			// /proc, /dev, /tmp and overlays using /mnt as scratch.
			name: "missing_mount_points",
			build: func(t *testing.T) string {
				root := t.TempDir()
				require.NoError(t, os.MkdirAll(filepath.Join(root, "bin"), 0o750))
				return root
			},
			wantErr: "/proc",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			repo := newRepo(t)
			if _, err := initTrustRoot(t, repo); err != nil {
				t.Fatalf("trust root: %v", err)
			}
			out, err := runBase(t, repo, "set", tt.build(t), "--guest-bin-dir", fakeGuestBins(t))
			require.Error(t, err, "an unbootable base must be refused, got %q", out)
			assert.Contains(t, err.Error()+out, tt.wantErr)
		})
	}
}

func TestSandboxBaseSet_IsIdempotentAndSurfacesAChangedTree(t *testing.T) {
	repo := newRepo(t)
	tree := userspaceTree(t)
	_, err := initTrustRoot(t, repo)
	require.NoError(t, err)

	bins := fakeGuestBins(t)
	first, err := runBase(t, repo, "set", tree, "--guest-bin-dir", bins)
	require.NoError(t, err)
	second, err := runBase(t, repo, "set", tree, "--guest-bin-dir", bins)
	require.NoError(t, err)
	// Re-running against an unchanged tree must pin the same digest, or every
	// re-registration would look like a substitution.
	assert.Equal(t, digestOf(t, first), digestOf(t, second),
		"re-registering an unchanged base changed its digest")

	// A changed tree must produce a DIFFERENT digest — visible, never silent.
	require.NoError(t, os.WriteFile(filepath.Join(tree, "bin", "extra"), []byte("x"), 0o600))
	third, err := runBase(t, repo, "set", tree, "--guest-bin-dir", bins)
	require.NoError(t, err)
	assert.NotEqual(t, digestOf(t, first), digestOf(t, third),
		"a changed base kept its old digest; a swap would go unnoticed")
}

// digestOf extracts the sha256 digest from a printed reference.
func digestOf(t *testing.T, out string) string {
	t.Helper()
	i := strings.Index(out, "sha256:")
	require.GreaterOrEqual(t, i, 0, "no digest in %q", out)
	return strings.TrimSpace(out[i:])
}

// fakeGuestBins stands in for a directory of prebuilt LINUX guest binaries —
// the path that works on a host install, which carries neither the mgit
// source nor a Go toolchain.
func fakeGuestBins(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	for _, n := range []string{"mgit", "mgit-guest"} {
		require.NoError(t, os.WriteFile(filepath.Join(dir, n), []byte("ELF-"+n), 0o600))
	}
	return dir
}

// initTrustRoot creates the repo's image-signing trust root, which a base
// must be signed into.
func initTrustRoot(t *testing.T, repo string) (string, error) {
	t.Helper()
	t.Chdir(repo)
	return runCLIOut(t, "sandbox", "image", "init")
}

// runBase executes `sandbox base <args...>` from inside repo.
func runBase(t *testing.T, repo string, args ...string) (string, error) {
	t.Helper()
	t.Chdir(repo)
	return runCLIOut(t, append([]string{"sandbox", "base"}, args...)...)
}

// TestSandboxBaseFrom_ComposesFromAnOCIImage covers the GA-critical path:
// the user pulls a public image, we compose it into a guest base, inject our
// binaries, pin the composed tree and sign it. mgit redistributes nothing.
// Refs: MGIT-61.15
func TestSandboxBaseFrom_ComposesFromAnOCIImage(t *testing.T) {
	srv, ref := fakeImageServer(t, map[string]string{
		"bin/sh":                         "#!/bin/sh",
		"etc/os-release":                 "ID=debian\nNAME=Debian",
		"lib/x86_64-linux-gnu/libc.so.6": "glibc",
	})
	defer srv.Close()

	repo := newRepo(t)
	_, err := initTrustRoot(t, repo)
	require.NoError(t, err)

	out, err := runBase(t, repo, "from", ref,
		"--guest-bin-dir", fakeGuestBins(t), "--plain-http")
	require.NoError(t, err, "base from: %s", out)

	// The composed tree is pinned by its own digest, not the image's: what
	// boots is the tree AFTER our binaries were injected.
	assert.Contains(t, out, "sha256:", "the composed base must be pinned, got %q", out)
	// Provenance: the OCI reference must be recorded too, so a base can be
	// traced back to the image it came from.
	assert.Contains(t, out, "acme/base", "the OCI source must be recorded, got %q", out)

	// Real images vary in which empty mount points they ship, and this
	// directory is one WE composed — so creating them is our job, not a
	// reason to reject a perfectly good image.
	for _, dir := range guestBaseMountDirs {
		assert.DirExists(t, filepath.Join(repo, ".mgit", "sandbox", "base", dir),
			"base from must create the guest mount point /%s", dir)
	}
}

func TestSandboxBaseFrom_RefusesAnImageThatIsNotALinuxUserspace(t *testing.T) {
	// A scratch/distroless image is one binary, not a userspace. An agent in
	// it cannot run a shell command — which is the entire point of a sandbox —
	// so refusing at compose time beats an inscrutable exec failure at task
	// time, hours later.
	srv, ref := fakeImageServer(t, map[string]string{"opt/app/server": "ELF"})
	defer srv.Close()

	repo := newRepo(t)
	_, err := initTrustRoot(t, repo)
	require.NoError(t, err)

	out, err := runBase(t, repo, "from", ref,
		"--guest-bin-dir", fakeGuestBins(t), "--plain-http")
	require.Error(t, err, "an image with no shell must be refused: %s", out)
	assert.Contains(t, err.Error(), "shell",
		"the refusal must say what is missing, got %q", err)
}

// TestSandboxBaseFrom_WarnsWhenTheBaseLibcWillNotMatchTheToolchain covers the
// single most confusing failure in a musl guest: a glibc-linked tool exits
// with "no such file or directory" naming its DYNAMIC LOADER, not the binary
// the user ran. Saying so up front costs one line; not saying it costs an hour.
// Refs: MGIT-61.15 req 6
func TestSandboxBaseFrom_WarnsWhenTheBaseLibcWillNotMatchTheToolchain(t *testing.T) {
	tests := []struct {
		name     string
		files    map[string]string
		wantWarn string
	}{
		{
			name: "musl_base_warns",
			files: map[string]string{
				"bin/sh": "#!/bin/sh", "lib/ld-musl-aarch64.so.1": "musl",
			},
			wantWarn: "musl",
		},
		{
			name: "glibc_base_is_quiet",
			files: map[string]string{
				"bin/sh": "#!/bin/sh", "lib/x86_64-linux-gnu/libc.so.6": "glibc",
			},
			wantWarn: "",
		},
		{
			name:     "no_libc_at_all_warns",
			files:    map[string]string{"bin/sh": "#!/bin/sh"},
			wantWarn: "no C library",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			srv, ref := fakeImageServer(t, tt.files)
			defer srv.Close()

			repo := newRepo(t)
			_, err := initTrustRoot(t, repo)
			require.NoError(t, err)

			out, err := runBase(t, repo, "from", ref,
				"--guest-bin-dir", fakeGuestBins(t), "--plain-http")
			require.NoError(t, err, "base from: %s", out)

			if tt.wantWarn == "" {
				assert.NotContains(t, out, "warning:",
					"a coherent base must not be warned about, got %q", out)
				return
			}
			assert.Contains(t, out, tt.wantWarn, "expected a libc warning, got %q", out)
		})
	}
}

// TestSandboxBaseFrom_RerunningTheSameRefPinsTheSameTree covers the
// requirement that makes a pin worth having: re-composing from the same image
// must produce the same digest, and a DIFFERENT image must produce a
// different one. A pin that drifts on an honest re-pull teaches people to
// ignore it; one that does not move when the contents change protects
// nothing. Refs: MGIT-61.15 req 2
func TestSandboxBaseFrom_RerunningTheSameRefPinsTheSameTree(t *testing.T) {
	files := map[string]string{"bin/sh": "#!/bin/sh", "etc/os-release": "ID=debian"}
	srv, ref := fakeImageServer(t, files)
	defer srv.Close()

	repo := newRepo(t)
	_, err := initTrustRoot(t, repo)
	require.NoError(t, err)
	bins := fakeGuestBins(t)

	compose := func(ref string) string {
		t.Helper()
		out, err := runBase(t, repo, "from", ref, "--guest-bin-dir", bins, "--plain-http", "--json")
		return pinnedRefFrom(t, out, err)
	}
	first := compose(ref)
	second := compose(ref)

	assert.Equal(t, first, second, "re-composing the same image must pin identically")

	// A different image must be a visible change, never a silent one. The
	// comparison is on the TREE digest alone: the source reference differs
	// too, and a test that watched the whole output would pass even if the
	// digest had not moved.
	other, otherRef := fakeImageServer(t, map[string]string{"bin/sh": "#!/bin/sh", "etc/os-release": "ID=alpine"})
	defer other.Close()
	changed := compose(otherRef)
	assert.NotEqual(t, first, changed, "different contents must pin differently")
}

// pinnedRefFrom extracts the digest-pinned reference from a --json run.
func pinnedRefFrom(t *testing.T, out string, err error) string {
	t.Helper()
	require.NoError(t, err, "base from: %s", out)
	var got struct {
		ImageRef string `json:"image_ref"`
	}
	// The JSON line is preceded by the pull's progress lines.
	start := strings.Index(out, "{")
	require.GreaterOrEqual(t, start, 0, "no JSON object in the output: %q", out)
	require.NoError(t, json.Unmarshal([]byte(out[start:]), &got), "output was %q", out)
	require.Contains(t, got.ImageRef, "sha256:")
	return got.ImageRef
}

// TestSandboxBaseFrom_RefusesAnImageBuiltForAnotherArchitecture pins the
// refusal a user hits when they copy an image reference from a colleague on a
// different machine.
//
// libkrun uses hardware virtualization: there is no emulation to cross
// architectures with, so an amd64 image on an arm64 Mac cannot run at all. The
// refusal names BOTH architectures — the one the image has and the one this
// host needs — because "no matching manifest" sends people looking for a
// network problem. Refs: MGIT-61.15 req 5
func TestSandboxBaseFrom_RefusesAnImageBuiltForAnotherArchitecture(t *testing.T) {
	other := "amd64"
	if runtime.GOARCH == "amd64" {
		other = "arm64"
	}
	srv, ref := fakeMultiArchImageServer(t, map[string]string{"bin/sh": "#!/bin/sh"}, other)
	defer srv.Close()

	repo := newRepo(t)
	_, err := initTrustRoot(t, repo)
	require.NoError(t, err)

	out, err := runBase(t, repo, "from", ref,
		"--guest-bin-dir", fakeGuestBins(t), "--plain-http")
	require.Error(t, err, "a wrong-architecture image must be refused: %s", out)
	assert.Contains(t, err.Error(), other, "the refusal must name what the image offers")
	assert.Contains(t, err.Error(), runtime.GOARCH, "the refusal must name what this host needs")
}

// TestSandboxBaseFrom_PicksThisHostsArchitectureFromAMultiArchImage is the
// positive control for the same code path: real distro images are index
// (multi-arch) images, so this is the ordinary case, not an edge one.
func TestSandboxBaseFrom_PicksThisHostsArchitectureFromAMultiArchImage(t *testing.T) {
	srv, ref := fakeMultiArchImageServer(t,
		map[string]string{"bin/sh": "#!/bin/sh"}, "amd64", "arm64")
	defer srv.Close()

	repo := newRepo(t)
	_, err := initTrustRoot(t, repo)
	require.NoError(t, err)

	out, err := runBase(t, repo, "from", ref,
		"--guest-bin-dir", fakeGuestBins(t), "--plain-http")
	require.NoError(t, err, "base from: %s", out)
	assert.Contains(t, out, "sha256:")
}

// fakeImageServer serves a one-layer OCI image over plain HTTP and returns
// the reference naming it. It keeps these tests offline.
func fakeImageServer(t *testing.T, files map[string]string) (*httptest.Server, string) {
	t.Helper()
	arch := "amd64"
	if runtime.GOARCH == "arm64" {
		arch = "arm64"
	}
	return serveImage(t, files, "", arch)
}

// fakeMultiArchImageServer serves an OCI INDEX listing one manifest per
// architecture — the shape every real distro image has.
func fakeMultiArchImageServer(t *testing.T, files map[string]string, arches ...string) (*httptest.Server, string) {
	t.Helper()
	return serveImage(t, files, "index", arches...)
}

// serveImage builds a registry serving one layer as either a plain manifest
// (kind "") or an index over per-architecture manifests (kind "index").
func serveImage(t *testing.T, files map[string]string, kind string, arches ...string) (*httptest.Server, string) {
	t.Helper()
	blobs := map[string][]byte{}
	manifests := map[string][]byte{}

	add := func(b []byte) string {
		sum := sha256.Sum256(b)
		d := "sha256:" + hex.EncodeToString(sum[:])
		blobs[d] = b
		return d
	}

	var buf bytes.Buffer
	zw := gzip.NewWriter(&buf)
	tw := tar.NewWriter(zw)
	for name, content := range files {
		require.NoError(t, tw.WriteHeader(&tar.Header{
			Name: name, Mode: 0o644, Size: int64(len(content)), Typeflag: tar.TypeReg,
		}))
		_, err := tw.Write([]byte(content))
		require.NoError(t, err)
	}
	require.NoError(t, tw.Close())
	require.NoError(t, zw.Close())

	layerDigest := add(buf.Bytes())

	manifestFor := func(arch string) []byte {
		configDigest := add([]byte(`{"architecture":"` + arch + `","os":"linux"}`))
		doc, err := json.Marshal(map[string]any{
			"schemaVersion": 2,
			"mediaType":     "application/vnd.oci.image.manifest.v1+json",
			"config":        map[string]any{"digest": configDigest, "size": 40},
			"layers": []map[string]any{
				{"mediaType": "application/vnd.oci.image.layer.v1.tar+gzip",
					"digest": layerDigest, "size": buf.Len()},
			},
		})
		require.NoError(t, err)
		return doc
	}

	if kind == "index" {
		entries := make([]map[string]any, 0, len(arches))
		for _, a := range arches {
			doc := manifestFor(a)
			d := add(doc)
			manifests[strings.TrimPrefix(d, "sha256:")] = doc
			entries = append(entries, map[string]any{
				"mediaType": "application/vnd.oci.image.manifest.v1+json",
				"digest":    d, "size": len(doc),
				"platform": map[string]any{"architecture": a, "os": "linux"},
			})
		}
		index, err := json.Marshal(map[string]any{
			"schemaVersion": 2,
			"mediaType":     "application/vnd.oci.image.index.v1+json",
			"manifests":     entries,
		})
		require.NoError(t, err)
		manifests["v1"] = index
	} else {
		manifests["v1"] = manifestFor(arches[0])
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/v2/", func(w http.ResponseWriter, r *http.Request) {
		path := strings.TrimPrefix(r.URL.Path, "/v2/")
		key := path[strings.LastIndex(path, "/")+1:]
		if strings.Contains(path, "/manifests/") {
			doc, ok := manifests[strings.TrimPrefix(key, "sha256:")]
			if !ok {
				doc, ok = manifests[key]
			}
			if !ok {
				w.WriteHeader(http.StatusNotFound)
				return
			}
			w.Header().Set("Content-Type", "application/vnd.oci.image.manifest.v1+json")
			if bytes.Contains(doc, []byte("image.index")) {
				w.Header().Set("Content-Type", "application/vnd.oci.image.index.v1+json")
			}
			_, _ = w.Write(doc)
			return
		}
		blob, ok := blobs[key]
		if !ok {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		_, _ = w.Write(blob)
	})
	srv := httptest.NewServer(mux)
	return srv, strings.TrimPrefix(srv.URL, "http://") + "/acme/base:v1"
}
