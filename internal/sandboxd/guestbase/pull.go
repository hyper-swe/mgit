package guestbase

import (
	"archive/tar"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"time"
)

// Media types this client understands. Both the OCI types and Docker's
// older equivalents are accepted, because most public images on Docker Hub
// are still served with the Docker types.
const (
	mediaTypeOCIIndex        = "application/vnd.oci.image.index.v1+json"
	mediaTypeOCIManifest     = "application/vnd.oci.image.manifest.v1+json"
	mediaTypeOCILayerGzip    = "application/vnd.oci.image.layer.v1.tar+gzip"
	mediaTypeDockerList      = "application/vnd.docker.distribution.manifest.list.v2+json"
	mediaTypeDockerManifest  = "application/vnd.docker.distribution.manifest.v2+json"
	mediaTypeDockerLayerGzip = "application/vnd.docker.image.rootfs.diff.tar.gzip"
)

// pullTimeout bounds a whole pull. Images are large and registries are
// sometimes slow, but an unbounded pull would hang a user's terminal with no
// explanation.
const pullTimeout = 15 * time.Minute

// maxLayerBytes caps a single decompressed layer. Without it a malicious or
// corrupt image could fill the host disk during an ordinary `base from` —
// the guest boundary does not help here, because extraction happens
// host-side before any VM exists.
const maxLayerBytes = 8 << 30 // 8 GiB

// PullOptions tunes a pull.
type PullOptions struct {
	// PlainHTTP talks to the registry over http. It exists for the test
	// registry and for local, unencrypted mirrors; production pulls are
	// https, which is the default.
	PlainHTTP bool
	// HTTPClient overrides the client, for tests and for callers that need
	// their own transport. nil uses a client bounded by pullTimeout.
	HTTPClient *http.Client
	// Progress, when set, is called with human-readable progress lines.
	Progress func(string)
}

// descriptor is one manifest or blob reference inside a manifest document.
type descriptor struct {
	MediaType string    `json:"mediaType"`
	Digest    string    `json:"digest"`
	Size      int64     `json:"size"`
	Platform  *platform `json:"platform,omitempty"`
}

// platform is an index entry's target, used to pick the host's architecture.
type platform struct {
	OS           string `json:"os"`
	Architecture string `json:"architecture"`
	Variant      string `json:"variant,omitempty"`
}

// manifestDoc covers both an image manifest and an index; which one it is is
// decided by whether Manifests or Layers is populated.
type manifestDoc struct {
	SchemaVersion int          `json:"schemaVersion"`
	MediaType     string       `json:"mediaType"`
	Config        descriptor   `json:"config"`
	Layers        []descriptor `json:"layers"`
	Manifests     []descriptor `json:"manifests"`
}

// Pull fetches an image and extracts its layers into destDir, returning the
// reference with Digest set to the manifest the registry actually served.
//
// destDir is created if absent and is extracted into as-is: composing a base
// means the caller may already have put things there, and layers are applied
// in order exactly as a container runtime would.
// Refs: MGIT-61.15, FR-17.17
func Pull(ctx context.Context, ref Ref, destDir string, opts PullOptions) (Ref, error) {
	ctx, cancel := context.WithTimeout(ctx, pullTimeout)
	defer cancel()

	c := &client{
		http:      opts.HTTPClient,
		scheme:    "https",
		plainHTTP: opts.PlainHTTP,
		progress:  opts.Progress,
	}
	if c.http == nil {
		c.http = &http.Client{Timeout: pullTimeout}
	}
	if opts.PlainHTTP {
		c.scheme = "http"
	}

	manifest, resolved, err := c.resolveManifest(ctx, ref)
	if err != nil {
		return Ref{}, err
	}
	if err := os.MkdirAll(destDir, 0o750); err != nil {
		return Ref{}, fmt.Errorf("guest base: create %s: %w", destDir, err)
	}
	for i, layer := range manifest.Layers {
		c.reportf("extracting layer %d/%d (%s)", i+1, len(manifest.Layers), shortDigest(layer.Digest))
		if err := c.extractLayer(ctx, ref, layer, destDir); err != nil {
			return Ref{}, err
		}
	}

	// Record the PATH the image declared for itself, so a toolchain it
	// installed outside the distro's default directories is visible to
	// commands run in it. A config we cannot read is not fatal: the base is
	// complete without it and the guest falls back to its built-in default,
	// which is exactly the behavior every base had before. Refs: MGIT-152
	cfg, err := c.fetchImageConfig(ctx, ref, manifest.Config)
	if err != nil {
		c.reportf("image config unreadable (%v); the guest will use its default PATH", err)
		return resolved, nil
	}
	if declared := pathFromImageConfig(cfg); declared != "" {
		if err := writeDeclaredEnv(destDir, declared); err != nil {
			return Ref{}, err
		}
		c.reportf("image declares PATH=%s", declared)
	}
	return resolved, nil
}

// client carries the per-pull HTTP state, including the bearer token a
// registry hands out after its 401 challenge.
type client struct {
	http      *http.Client
	scheme    string
	plainHTTP bool
	token     string
	progress  func(string)
}

func (c *client) reportf(format string, args ...any) {
	if c.progress != nil {
		c.progress(fmt.Sprintf(format, args...))
	}
}

// resolveManifest fetches the reference's manifest, following an index to the
// entry matching the host architecture. It returns the image manifest and the
// reference with the RESOLVED digest — the thing worth recording, since a tag
// can move.
func (c *client) resolveManifest(ctx context.Context, ref Ref) (manifestDoc, Ref, error) {
	target := ref.Tag
	if ref.Digest != "" {
		target = ref.Digest
	}
	doc, digest, err := c.fetchManifest(ctx, ref, target)
	if err != nil {
		return manifestDoc{}, Ref{}, err
	}

	// An index lists per-platform manifests; pick the host's and fetch it.
	if len(doc.Manifests) > 0 {
		pick, err := selectPlatform(doc.Manifests)
		if err != nil {
			return manifestDoc{}, Ref{}, err
		}
		c.reportf("index: selected %s", shortDigest(pick.Digest))
		doc, digest, err = c.fetchManifest(ctx, ref, pick.Digest)
		if err != nil {
			return manifestDoc{}, Ref{}, err
		}
	}
	if len(doc.Layers) == 0 {
		return manifestDoc{}, Ref{}, fmt.Errorf(
			"guest base: %s resolved to a manifest with no layers", ref)
	}

	resolved := ref
	resolved.Digest = digest
	return doc, resolved, nil
}

// selectPlatform picks the index entry matching this host's guest
// architecture, and names both sides when there is none — a mystery boot
// failure later is far worse than a clear refusal now (MGIT-61.15 req 5).
func selectPlatform(entries []descriptor) (descriptor, error) {
	want := hostOCIArch()
	var available []string
	for _, e := range entries {
		if e.Platform == nil {
			continue
		}
		if e.Platform.OS == "linux" && e.Platform.Architecture == want {
			return e, nil
		}
		available = append(available, e.Platform.OS+"/"+e.Platform.Architecture)
	}
	return descriptor{}, fmt.Errorf(
		"guest base: image has no linux/%s manifest; the guest runs the host's "+
			"architecture because libkrun uses hardware virtualization, with no "+
			"emulation to cross architectures. Available: %s",
		want, strings.Join(available, ", "))
}

// hostOCIArch maps Go's GOARCH to the OCI architecture name.
func hostOCIArch() string {
	switch runtime.GOARCH {
	case "amd64":
		return "amd64"
	case "arm64":
		return "arm64"
	default:
		return runtime.GOARCH
	}
}

// fetchManifest GETs one manifest and returns it with its digest.
func (c *client) fetchManifest(ctx context.Context, ref Ref, target string) (manifestDoc, string, error) {
	url := fmt.Sprintf("%s://%s/v2/%s/manifests/%s", c.scheme, ref.Registry, ref.Repository, target)
	body, err := c.get(ctx, url, ref, []string{
		mediaTypeOCIIndex, mediaTypeOCIManifest, mediaTypeDockerList, mediaTypeDockerManifest,
	})
	if err != nil {
		return manifestDoc{}, "", err
	}
	defer func() { _ = body.Close() }()

	raw, err := io.ReadAll(io.LimitReader(body, 32<<20))
	if err != nil {
		return manifestDoc{}, "", fmt.Errorf("guest base: read manifest for %s: %w", ref, err)
	}
	sum := sha256.Sum256(raw)
	digest := "sha256:" + hex.EncodeToString(sum[:])

	// When the reference pinned a digest, what came back must match it.
	if strings.HasPrefix(target, "sha256:") && digest != target {
		return manifestDoc{}, "", fmt.Errorf(
			"guest base: manifest for %s hashes to %s, requested %s", ref, digest, target)
	}

	var doc manifestDoc
	if err := json.Unmarshal(raw, &doc); err != nil {
		return manifestDoc{}, "", fmt.Errorf("guest base: parse manifest for %s: %w", ref, err)
	}
	return doc, digest, nil
}

// extractLayer streams one layer blob, verifying its digest as it reads, and
// untars it into destDir.
func (c *client) extractLayer(ctx context.Context, ref Ref, layer descriptor, destDir string) error {
	url := fmt.Sprintf("%s://%s/v2/%s/blobs/%s", c.scheme, ref.Registry, ref.Repository, layer.Digest)
	body, err := c.get(ctx, url, ref, nil)
	if err != nil {
		return err
	}
	defer func() { _ = body.Close() }()

	// Hash WHILE extracting rather than downloading then checking: the digest
	// is verified after the stream is consumed, and a mismatch fails the pull.
	hasher := sha256.New()
	tee := io.TeeReader(io.LimitReader(body, maxLayerBytes), hasher)

	zr, err := gzip.NewReader(tee)
	if err != nil {
		return fmt.Errorf("guest base: layer %s is not gzip: %w", shortDigest(layer.Digest), err)
	}
	defer func() { _ = zr.Close() }()

	if err := extractTar(zr, destDir); err != nil {
		return err
	}
	// Drain any trailing bytes so the hash covers the whole blob.
	if _, err := io.Copy(io.Discard, tee); err != nil {
		return fmt.Errorf("guest base: read layer %s: %w", shortDigest(layer.Digest), err)
	}
	got := "sha256:" + hex.EncodeToString(hasher.Sum(nil))
	if got != layer.Digest {
		return fmt.Errorf(
			"guest base: layer digest mismatch: got %s, manifest says %s — the registry "+
				"served different bytes than were requested", got, layer.Digest)
	}
	return nil
}

// get performs an authenticated GET, following a registry's bearer-token
// challenge once.
func (c *client) get(ctx context.Context, url string, ref Ref, accept []string) (io.ReadCloser, error) {
	resp, err := c.do(ctx, url, accept)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode == http.StatusUnauthorized {
		challenge := resp.Header.Get("WWW-Authenticate")
		_ = resp.Body.Close()
		if err := c.authenticate(ctx, challenge, ref); err != nil {
			return nil, err
		}
		if resp, err = c.do(ctx, url, accept); err != nil {
			return nil, err
		}
	}
	if resp.StatusCode != http.StatusOK {
		_ = resp.Body.Close()
		return nil, fmt.Errorf("guest base: %s returned %s", url, resp.Status)
	}
	return resp.Body, nil
}

// do issues one request with the current token, if any.
func (c *client) do(ctx context.Context, url string, accept []string) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, fmt.Errorf("guest base: request %s: %w", url, err)
	}
	for _, a := range accept {
		req.Header.Add("Accept", a)
	}
	if c.token != "" {
		req.Header.Set("Authorization", "Bearer "+c.token)
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("guest base: fetch %s: %w", url, err)
	}
	return resp, nil
}

// authenticate follows a Bearer challenge to obtain an anonymous pull token.
// Only anonymous pulls are supported: a base is public tooling, and holding
// registry credentials is not something mgit should start doing.
func (c *client) authenticate(ctx context.Context, challenge string, ref Ref) error {
	if !strings.HasPrefix(challenge, "Bearer ") {
		return fmt.Errorf(
			"guest base: %s requires authentication mgit does not perform (%q); "+
				"only anonymous pulls of public images are supported", ref.Registry, challenge)
	}
	params := parseChallenge(strings.TrimPrefix(challenge, "Bearer "))
	realm := params["realm"]
	if realm == "" {
		return fmt.Errorf("guest base: auth challenge from %s has no realm", ref.Registry)
	}
	url := realm + "?scope=repository:" + ref.Repository + ":pull"
	if svc := params["service"]; svc != "" {
		url += "&service=" + svc
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return fmt.Errorf("guest base: token request: %w", err)
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return fmt.Errorf("guest base: fetch token from %s: %w", realm, err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("guest base: token endpoint %s returned %s", realm, resp.Status)
	}
	var tok struct {
		Token       string `json:"token"`
		AccessToken string `json:"access_token"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&tok); err != nil {
		return fmt.Errorf("guest base: parse token: %w", err)
	}
	c.token = tok.Token
	if c.token == "" {
		c.token = tok.AccessToken
	}
	if c.token == "" {
		return fmt.Errorf("guest base: %s issued an empty token", realm)
	}
	return nil
}

// parseChallenge splits a WWW-Authenticate parameter list.
func parseChallenge(s string) map[string]string {
	out := map[string]string{}
	for _, part := range strings.Split(s, ",") {
		k, v, ok := strings.Cut(strings.TrimSpace(part), "=")
		if !ok {
			continue
		}
		out[k] = strings.Trim(v, `"`)
	}
	return out
}

// shortDigest abbreviates a digest for progress messages.
func shortDigest(d string) string {
	if i := strings.IndexByte(d, ':'); i >= 0 && len(d) > i+13 {
		return d[i+1 : i+13]
	}
	return d
}

// whiteoutPrefix marks a path deleted by an upper layer; opaqueWhiteout
// clears a whole directory. Applying them is what makes a multi-layer image
// extract to the tree the user actually asked for.
const (
	whiteoutPrefix = ".wh."
	opaqueWhiteout = ".wh..wh..opq"
)

// extractTar writes one decompressed layer into destDir, applying whiteouts
// and refusing any entry that would escape.
func extractTar(r io.Reader, destDir string) error {
	root, err := filepath.Abs(destDir)
	if err != nil {
		return fmt.Errorf("guest base: resolve %s: %w", destDir, err)
	}
	tr := tar.NewReader(r)
	for {
		hdr, err := tr.Next()
		if errors.Is(err, io.EOF) {
			return nil
		}
		if err != nil {
			return fmt.Errorf("guest base: read layer: %w", err)
		}
		target, err := safeJoin(root, hdr.Name)
		if err != nil {
			return err
		}
		if handled, err := applyWhiteout(root, hdr.Name); err != nil {
			return err
		} else if handled {
			continue
		}
		if err := writeEntry(tr, hdr, target, root); err != nil {
			return err
		}
	}
}

// safeJoin resolves a tar entry against the destination, refusing anything
// that escapes it. A layer entry writing outside the base would land in the
// host filesystem during an ordinary pull, before any VM exists.
func safeJoin(root, name string) (string, error) {
	clean := filepath.Clean(filepath.Join(root, name))
	if clean != root && !strings.HasPrefix(clean, root+string(os.PathSeparator)) {
		return "", fmt.Errorf(
			"guest base: layer entry %q escapes the base directory; refusing to extract", name)
	}
	return clean, nil
}

// applyWhiteout deletes what an upper layer marks as removed, reporting
// whether the entry was a whiteout marker (and so must not be written).
func applyWhiteout(root, name string) (bool, error) {
	base := filepath.Base(name)
	if !strings.HasPrefix(base, whiteoutPrefix) {
		return false, nil
	}
	dir := filepath.Dir(name)
	if base == opaqueWhiteout {
		// Opaque: everything already in this directory is hidden.
		target, err := safeJoin(root, dir)
		if err != nil {
			return true, err
		}
		entries, err := os.ReadDir(target)
		if err != nil {
			// Nothing has been extracted at that path yet, so there is
			// nothing for the opaque marker to hide. Not an error: an opaque
			// whiteout in the FIRST layer is legitimate and common.
			//nolint:nilerr // an absent directory means the whiteout is a no-op
			return true, nil
		}
		for _, e := range entries {
			if err := os.RemoveAll(filepath.Join(target, e.Name())); err != nil {
				return true, fmt.Errorf("guest base: apply opaque whiteout in %s: %w", dir, err)
			}
		}
		return true, nil
	}
	target, err := safeJoin(root, filepath.Join(dir, strings.TrimPrefix(base, whiteoutPrefix)))
	if err != nil {
		return true, err
	}
	if err := os.RemoveAll(target); err != nil {
		return true, fmt.Errorf("guest base: apply whiteout %s: %w", name, err)
	}
	return true, nil
}

// writeEntry materializes one tar entry.
func writeEntry(tr io.Reader, hdr *tar.Header, target, root string) error {
	switch hdr.Typeflag {
	case tar.TypeDir:
		return os.MkdirAll(target, 0o750)

	case tar.TypeReg:
		if err := os.MkdirAll(filepath.Dir(target), 0o750); err != nil {
			return fmt.Errorf("guest base: create %s: %w", filepath.Dir(target), err)
		}
		// Preserve only the executable bit: a base's tools must stay
		// runnable, but the rest of the mode is not something we carry.
		mode := os.FileMode(0o600)
		if hdr.FileInfo().Mode()&0o111 != 0 {
			mode = 0o700
		}
		// A layer may replace a file from a lower layer.
		if err := os.RemoveAll(target); err != nil {
			return fmt.Errorf("guest base: replace %s: %w", hdr.Name, err)
		}
		f, err := os.OpenFile(target, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, mode) //nolint:gosec // path checked by safeJoin
		if err != nil {
			return fmt.Errorf("guest base: create %s: %w", hdr.Name, err)
		}
		defer func() { _ = f.Close() }()
		if _, err := io.Copy(f, io.LimitReader(tr, maxLayerBytes)); err != nil {
			return fmt.Errorf("guest base: write %s: %w", hdr.Name, err)
		}
		return nil

	case tar.TypeSymlink:
		// Symlink TARGETS are not resolved here: a base legitimately contains
		// links to absolute paths that only exist inside the guest (/bin/sh
		// -> /usr/bin/busybox). What matters is that the LINK ITSELF is
		// written inside the tree, which safeJoin already established.
		if err := os.MkdirAll(filepath.Dir(target), 0o750); err != nil {
			return err
		}
		_ = os.RemoveAll(target)
		return os.Symlink(hdr.Linkname, target)

	case tar.TypeLink:
		source, err := safeJoin(root, hdr.Linkname)
		if err != nil {
			return err
		}
		if err := os.MkdirAll(filepath.Dir(target), 0o750); err != nil {
			return err
		}
		_ = os.RemoveAll(target)
		return os.Link(source, target)

	default:
		// Devices, fifos and sockets are skipped rather than refused: images
		// commonly carry /dev nodes that a virtio-fs guest does not use, and
		// creating them needs privileges a pull should not want.
		return nil
	}
}
