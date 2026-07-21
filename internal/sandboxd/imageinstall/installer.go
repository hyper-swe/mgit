// Package imageinstall turns a shipped, pinned guest-image bundle into a
// registered, digest-pinned, signed image the sandbox can boot — the one
// step that makes the sandbox "active" without a manual kernel/rootfs build
// (MGIT-61.1). It fetches the host platform's kernel + rootfs named by a
// manifest, verifies each against the manifest's sha256 (distribution
// integrity), places them at stable paths under the host config root
// (images.lock stores absolute paths, read at boot), ensures the local
// Ed25519 trust root exists, and registers the image (local-trust, MGIT-61).
package imageinstall

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strings"

	"github.com/hyper-swe/mgit/internal/sandboxd/images"
)

// Manifest describes the images a bundle publishes, keyed by "os/arch".
// Refs: MGIT-61.1
type Manifest struct {
	Schema int                      `json:"schema"`
	Images map[string]PlatformImage `json:"images"`
}

// PlatformImage names one platform's kernel + rootfs artifacts, their pinned
// sha256 digests, and the guest kernel command line. Refs: MGIT-61.1
type PlatformImage struct {
	Kernel       string `json:"kernel"`
	KernelSHA256 string `json:"kernel_sha256"`
	Rootfs       string `json:"rootfs"`
	RootfsSHA256 string `json:"rootfs_sha256"`
	Cmdline      string `json:"cmdline"`
}

// Installer resolves + verifies + registers a shipped guest image into a host
// config root's image set. It is host-side setup, not a daemon operation.
// Refs: MGIT-61.1
type Installer struct {
	// HostRoot is the config root holding images.lock + the trust root
	// (<repo>/.mgit/sandbox).
	HostRoot string
	// Platform overrides the target "os/arch" (default: this host).
	Platform string
	// Client fetches http(s) sources (default: http.DefaultClient). Unused
	// for local-directory sources.
	Client *http.Client
	// Audit records trust-root generation when one is auto-created.
	Audit images.TrustRootAuditor
}

// Result is the outcome of an install.
type Result struct {
	Ref        string // name@sha256:... to pass to the sandbox
	Platform   string // resolved os/arch
	KernelPath string
	RootfsPath string
}

// Install fetches the platform's image from source (a local directory or an
// http(s) base URL, each containing manifest.json + the named artifacts),
// verifies the sha256 of each artifact against the manifest, ensures the
// trust root, and registers the image under name. It fails closed on any
// digest mismatch and is idempotent (re-installing overwrites the artifacts
// and re-registers). Refs: MGIT-61.1
func (in *Installer) Install(ctx context.Context, source, name string) (*Result, error) {
	if in.HostRoot == "" {
		return nil, fmt.Errorf("image install: host root must not be empty")
	}
	if name == "" {
		return nil, fmt.Errorf("image install: image name must not be empty")
	}
	platform := in.Platform
	if platform == "" {
		platform = runtime.GOOS + "/" + runtime.GOARCH
	}

	manifestBytes, err := in.read(ctx, source, "manifest.json")
	if err != nil {
		return nil, fmt.Errorf("image install: read manifest: %w", err)
	}
	var m Manifest
	if err := json.Unmarshal(manifestBytes, &m); err != nil {
		return nil, fmt.Errorf("image install: parse manifest: %w", err)
	}
	img, ok := m.Images[platform]
	if !ok {
		return nil, fmt.Errorf("image install: manifest has no image for %s (available: %s)",
			platform, strings.Join(platformKeys(m), ", "))
	}

	destDir := filepath.Join(in.HostRoot, "images", name)
	if err := os.MkdirAll(destDir, 0o700); err != nil {
		return nil, fmt.Errorf("image install: create image dir: %w", err)
	}
	kernelPath := filepath.Join(destDir, filepath.Base(img.Kernel))
	rootfsPath := filepath.Join(destDir, filepath.Base(img.Rootfs))

	if err := in.fetchVerified(ctx, source, img.Kernel, kernelPath, img.KernelSHA256); err != nil {
		return nil, fmt.Errorf("image install: kernel: %w", err)
	}
	if err := in.fetchVerified(ctx, source, img.Rootfs, rootfsPath, img.RootfsSHA256); err != nil {
		return nil, fmt.Errorf("image install: rootfs: %w", err)
	}

	priv, err := images.LoadSigningKey(in.HostRoot)
	if err != nil {
		// No trust root yet: generate one so `install` is a single step
		// (equivalent to a first-time `image init`). Refs: FR-17.38
		priv, err = images.GenerateTrustRoot(ctx, in.HostRoot, in.Audit)
		if err != nil {
			return nil, fmt.Errorf("image install: initialize trust root: %w", err)
		}
	}

	entry, err := images.BuildEntry(kernelPath, rootfsPath, img.Cmdline)
	if err != nil {
		return nil, fmt.Errorf("image install: build entry: %w", err)
	}
	ref, err := images.Register(in.HostRoot, name, entry, priv)
	if err != nil {
		return nil, fmt.Errorf("image install: register: %w", err)
	}
	return &Result{Ref: ref, Platform: platform, KernelPath: kernelPath, RootfsPath: rootfsPath}, nil
}

// fetchVerified fetches rel from source into dst and verifies its sha256
// against want (fail-closed: a mismatch removes the file and errors). Refs: MGIT-61.1
func (in *Installer) fetchVerified(ctx context.Context, source, rel, dst, want string) error {
	if err := in.fetchTo(ctx, source, rel, dst); err != nil {
		return err
	}
	got, err := fileSHA256(dst)
	if err != nil {
		return err
	}
	if !sha256Equal(got, want) {
		_ = os.Remove(dst)
		return fmt.Errorf("sha256 mismatch for %s: got %s, want %s (refusing to install)", rel, got, normalizeSHA(want))
	}
	return nil
}

// read returns the bytes of rel under source (local dir or http(s) base).
func (in *Installer) read(ctx context.Context, source, rel string) ([]byte, error) {
	if isURL(source) {
		return in.httpGet(ctx, joinURL(source, rel))
	}
	return os.ReadFile(filepath.Join(source, rel)) //nolint:gosec // operator-supplied bundle path
}

// fetchTo copies/downloads rel under source into dst.
func (in *Installer) fetchTo(ctx context.Context, source, rel, dst string) error {
	if isURL(source) {
		data, err := in.httpGet(ctx, joinURL(source, rel))
		if err != nil {
			return err
		}
		return os.WriteFile(dst, data, 0o600)
	}
	return copyFile(filepath.Join(source, rel), dst)
}

// httpGet fetches a URL, returning its body or a clear error.
func (in *Installer) httpGet(ctx context.Context, url string) ([]byte, error) {
	client := in.Client
	if client == nil {
		client = http.DefaultClient
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, fmt.Errorf("request %s: %w", url, err)
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("fetch %s: %w", url, err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("fetch %s: HTTP %d", url, resp.StatusCode)
	}
	return io.ReadAll(resp.Body)
}

func platformKeys(m Manifest) []string {
	keys := make([]string, 0, len(m.Images))
	for k := range m.Images {
		keys = append(keys, k)
	}
	return keys
}

func isURL(s string) bool {
	return strings.HasPrefix(s, "http://") || strings.HasPrefix(s, "https://")
}

func joinURL(base, rel string) string {
	return strings.TrimSuffix(base, "/") + "/" + rel
}

// fileSHA256 returns the streamed "sha256:<hex>" of a file.
func fileSHA256(path string) (string, error) {
	f, err := os.Open(path) //nolint:gosec // host-owned image path
	if err != nil {
		return "", err
	}
	defer func() { _ = f.Close() }()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return "sha256:" + hex.EncodeToString(h.Sum(nil)), nil
}

// sha256Equal compares two sha256 strings, tolerating an optional "sha256:"
// prefix and case on either side.
func sha256Equal(a, b string) bool {
	return normalizeSHA(a) == normalizeSHA(b)
}

func normalizeSHA(s string) string {
	return strings.ToLower(strings.TrimPrefix(s, "sha256:"))
}

// copyFile copies src to dst (0600), streaming.
func copyFile(src, dst string) error {
	in, err := os.Open(src) //nolint:gosec // operator-supplied bundle path
	if err != nil {
		return err
	}
	defer func() { _ = in.Close() }()
	out, err := os.OpenFile(dst, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o600) //nolint:gosec // dst is under the host config root, derived from HostRoot
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		_ = out.Close()
		return err
	}
	return out.Close()
}
