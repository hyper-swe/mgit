package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/hyper-swe/mgit/internal/sandboxd/images"
)

// runImage executes the `image` command tree from inside repoDir (the
// commands resolve the host root from cwd), capturing stdout+stderr.
func runImage(t *testing.T, repoDir string, args ...string) (string, error) {
	t.Helper()
	t.Chdir(repoDir)
	cmd := sandboxImageCmd()
	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(&out)
	cmd.SetArgs(args)
	err := cmd.Execute()
	return out.String(), err
}

// newRepo creates a temp dir with a .mgit directory (an mgit repo root).
func newRepo(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	require.NoError(t, os.MkdirAll(filepath.Join(dir, ".mgit"), 0o700))
	return dir
}

// writeImageFiles creates stand-in kernel + rootfs files and returns paths.
func writeImageFiles(t *testing.T) (kernel, rootfs string) {
	t.Helper()
	dir := t.TempDir()
	kernel = filepath.Join(dir, "vmlinux")
	rootfs = filepath.Join(dir, "rootfs.img")
	require.NoError(t, os.WriteFile(kernel, []byte("kernel-bytes"), 0o600))
	require.NoError(t, os.WriteFile(rootfs, []byte("rootfs-bytes"), 0o600))
	return kernel, rootfs
}

func clockUTC() func() time.Time {
	return func() time.Time { return time.Date(2026, 6, 21, 12, 0, 0, 0, time.UTC) }
}

// TestImageAdd_RegistersAndResolves verifies a registered image is signed
// against the trust root and immediately resolvable by the sandbox store,
// and that the command prints the digest-pinned reference.
func TestImageAdd_RegistersAndResolves(t *testing.T) {
	repo := newRepo(t)
	hostRoot := filepath.Join(repo, ".mgit", "sandbox")
	// Set up the trust root via the CLI's own init command.
	_, err := runImage(t, repo, "init")
	require.NoError(t, err)

	kernel, rootfs := writeImageFiles(t)
	out, err := runImage(t, repo, "add",
		"--name", "go-node", "--kernel", kernel, "--rootfs", rootfs, "--cmdline", "console=hvc0 ro")
	require.NoError(t, err)
	require.Contains(t, out, "go-node@sha256:", "the digest-pinned reference is printed")

	ref := strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(out), "Registered "))
	store, err := images.NewStore(hostRoot, clockUTC())
	require.NoError(t, err)
	resolved, err := store.Resolve(ref)
	require.NoError(t, err, "a freshly registered image must resolve")
	assert.Equal(t, kernel, resolved.KernelPath)
	assert.Equal(t, rootfs, resolved.RootfsPath)
	assert.Equal(t, "console=hvc0 ro", resolved.Cmdline)
}

// TestImageAdd_JSON verifies --json emits the reference structurally.
func TestImageAdd_JSON(t *testing.T) {
	repo := newRepo(t)
	_, err := runImage(t, repo, "init")
	require.NoError(t, err)
	kernel, rootfs := writeImageFiles(t)
	out, err := runImage(t, repo, "add", "--name", "go-node", "--kernel", kernel, "--rootfs", rootfs, "--json")
	require.NoError(t, err)
	assert.Contains(t, out, `"image_ref"`)
	assert.Contains(t, out, "go-node@sha256:")
}

// TestImageAdd_MissingTrustRoot_FailsClosed verifies that without a trust
// root (no init), registration fails clearly rather than producing an
// unsigned/unresolvable image.
func TestImageAdd_MissingTrustRoot_FailsClosed(t *testing.T) {
	repo := newRepo(t)
	kernel, rootfs := writeImageFiles(t)
	_, err := runImage(t, repo, "add", "--name", "go-node", "--kernel", kernel, "--rootfs", rootfs)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "init", "the error points the operator at trust-root setup")
}

// TestImageAdd_MissingImageFile fails clearly when a referenced image file
// does not exist (BuildEntry cannot hash it).
func TestImageAdd_MissingImageFile(t *testing.T) {
	repo := newRepo(t)
	_, err := runImage(t, repo, "init")
	require.NoError(t, err)
	_, rootfs := writeImageFiles(t)
	_, err = runImage(t, repo, "add",
		"--name", "go-node", "--kernel", filepath.Join(t.TempDir(), "absent-vmlinux"), "--rootfs", rootfs)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "image add")
}

// TestImageAdd_MissingFlags rejects an incomplete invocation.
func TestImageAdd_MissingFlags(t *testing.T) {
	repo := newRepo(t)
	_, err := runImage(t, repo, "add", "--name", "go-node")
	require.Error(t, err)
}

// TestImageAdd_NotARepo fails clearly outside an mgit repository.
func TestImageAdd_NotARepo(t *testing.T) {
	dir := t.TempDir() // no .mgit
	kernel, rootfs := writeImageFiles(t)
	_, err := runImage(t, dir, "add", "--name", "go-node", "--kernel", kernel, "--rootfs", rootfs)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "not an mgit repository")
}

// TestImageInit_GeneratesTrustRoot verifies init creates a usable trust
// root (the image store opens against it) and prints the fingerprint.
func TestImageInit_GeneratesTrustRoot(t *testing.T) {
	repo := newRepo(t)
	out, err := runImage(t, repo, "init")
	require.NoError(t, err)
	assert.Contains(t, out, "fingerprint", "the new key fingerprint is surfaced")

	_, err = images.NewStore(filepath.Join(repo, ".mgit", "sandbox"), clockUTC())
	require.NoError(t, err, "the store opens once the trust root exists")
}

// TestImageCmd_Help lists the subcommands.
func TestImageCmd_Help(t *testing.T) {
	cmd := sandboxImageCmd()
	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetArgs([]string{"--help"})
	require.NoError(t, cmd.Execute())
	assert.Contains(t, out.String(), "init")
	assert.Contains(t, out.String(), "add")
}
