package libkrun

// Carry a patched libkrun in the macOS build; fixes MGIT-225.
// Refs: MGIT-259

import (
	"bytes"
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/hyper-swe/mgit/internal/model"
)

// touch writes a small file at path, creating its directory.
func touch(t *testing.T, path, body string) string {
	t.Helper()
	require.NoError(t, os.MkdirAll(filepath.Dir(path), 0o750))
	require.NoError(t, os.WriteFile(path, []byte(body), 0o600))
	return path
}

func TestRequireBundledLibkrun(t *testing.T) {
	tests := []struct {
		name    string
		layout  func(t *testing.T, root string) (exe string, loaded []string)
		wantErr []string
	}{
		{
			name: "the_copy_beside_the_daemon",
			layout: func(t *testing.T, root string) (string, []string) {
				exe := touch(t, filepath.Join(root, "bin", "mgit-sandboxd"), "daemon")
				return exe, []string{"/usr/lib/libSystem.B.dylib", touch(t, filepath.Join(root, "bin", "lib", "libkrun.1.dylib"), "krun")}
			},
		},
		{
			name: "the_install_layout",
			layout: func(t *testing.T, root string) (string, []string) {
				exe := touch(t, filepath.Join(root, "bin", "mgit-sandboxd"), "daemon")
				return exe, []string{touch(t, filepath.Join(root, "lib", "mgit", "libkrun.1.dylib"), "krun")}
			},
		},
		{
			name: "a_daemon_reached_through_a_symlink",
			layout: func(t *testing.T, root string) (string, []string) {
				real := touch(t, filepath.Join(root, "keg", "bin", "mgit-sandboxd"), "daemon")
				krun := touch(t, filepath.Join(root, "keg", "lib", "mgit", "libkrun.1.dylib"), "krun")
				link := filepath.Join(root, "linked", "bin", "mgit-sandboxd")
				require.NoError(t, os.MkdirAll(filepath.Dir(link), 0o750))
				require.NoError(t, os.Symlink(real, link))
				return link, []string{krun}
			},
		},
		{
			name: "the_bundled_file_under_another_spelling",
			layout: func(t *testing.T, root string) (string, []string) {
				exe := touch(t, filepath.Join(root, "bin", "mgit-sandboxd"), "daemon")
				touch(t, filepath.Join(root, "bin", "lib", "libkrun.1.dylib"), "krun")
				return exe, []string{filepath.Join(root, "bin", "..", "bin", "lib", "libkrun.1.dylib")}
			},
		},
		{
			name: "another_libkrun_is_refused",
			layout: func(t *testing.T, root string) (string, []string) {
				exe := touch(t, filepath.Join(root, "bin", "mgit-sandboxd"), "daemon")
				touch(t, filepath.Join(root, "bin", "lib", "libkrun.1.dylib"), "krun")
				return exe, []string{touch(t, filepath.Join(root, "homebrew", "lib", "libkrun.1.dylib"), "other")}
			},
			wantErr: []string{"homebrew", filepath.Join("bin", "lib", "libkrun.1.dylib"), "lib/", "release archive"},
		},
		{
			name: "a_copy_with_the_same_bytes_elsewhere_is_refused",
			layout: func(t *testing.T, root string) (string, []string) {
				exe := touch(t, filepath.Join(root, "bin", "mgit-sandboxd"), "daemon")
				touch(t, filepath.Join(root, "bin", "lib", "libkrun.1.dylib"), "krun")
				return exe, []string{touch(t, filepath.Join(root, "copy", "libkrun.1.dylib"), "krun")}
			},
			wantErr: []string{"copy"},
		},
		{
			name: "the_bundle_is_missing",
			layout: func(t *testing.T, root string) (string, []string) {
				exe := touch(t, filepath.Join(root, "bin", "mgit-sandboxd"), "daemon")
				return exe, []string{touch(t, filepath.Join(root, "usr", "local", "lib", "libkrun.1.dylib"), "krun")}
			},
			wantErr: []string{filepath.Join("usr", "local", "lib", "libkrun.1.dylib")},
		},
		{
			name: "no_libkrun_loaded",
			layout: func(t *testing.T, root string) (string, []string) {
				return touch(t, filepath.Join(root, "bin", "mgit-sandboxd"), "daemon"), []string{"/usr/lib/libSystem.B.dylib"}
			},
			wantErr: []string{"no libkrun"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			exe, loaded := tt.layout(t, t.TempDir())
			err := requireBundledLibkrun(loaded, exe, "libkrun.1.dylib")
			if tt.wantErr == nil {
				assert.NoError(t, err)
				return
			}
			require.Error(t, err)
			assert.ErrorIs(t, err, model.ErrSandboxBackendUnavailable)
			for _, want := range tt.wantErr {
				assert.Contains(t, err.Error(), want)
			}
		})
	}
}

func TestBundleCheck_AnUnstampedBuildAcceptsAnyLibkrun(t *testing.T) {
	c := bundleCheck{
		want:   "",
		loaded: func() []string { return []string{"/opt/homebrew/lib/libkrun.1.dylib"} },
		exe:    func() (string, error) { return "/usr/local/bin/mgit-sandboxd", nil },
	}
	assert.NoError(t, c.err())
}

func TestBundleCheck_AStampedBuildWhoseExecutableCannotBeResolved_IsRefused(t *testing.T) {
	c := bundleCheck{
		want:   "1",
		loaded: func() []string { return nil },
		exe:    func() (string, error) { return "", errors.New("no executable") },
	}
	err := c.err()
	require.Error(t, err)
	assert.ErrorIs(t, err, model.ErrSandboxBackendUnavailable)
}

// refusingBundle is a stamped build whose loader mapped a libkrun from
// somewhere other than beside the daemon.
func refusingBundle() bundleCheck {
	return bundleCheck{
		want:   "1",
		loaded: func() []string { return []string{"/opt/homebrew/Cellar/libkrun/1.19.4/lib/libkrun.1.19.4.dylib"} },
		exe:    func() (string, error) { return "/nonexistent/bin/mgit-sandboxd", nil },
	}
}

func TestNewHypervisor_ABundledBuildRefusesALibkrunThatIsNotItsOwn(t *testing.T) {
	_, err := newHypervisor(slog.New(slog.NewTextHandler(&bytes.Buffer{}, nil)), stubCapability{}, refusingBundle())
	require.Error(t, err)
	assert.ErrorIs(t, err, model.ErrSandboxBackendUnavailable)
	assert.Contains(t, err.Error(), "/opt/homebrew/Cellar/libkrun")
}

func TestChildMain_ABundledBuildRefusesALibkrunThatIsNotItsOwn(t *testing.T) {
	var handshake, stderr bytes.Buffer
	rc := childMainWith(strings.NewReader(`{}`), &handshake, &stderr, refusingBundle())
	assert.NotEqual(t, 0, rc)
	assert.Contains(t, handshake.String(), "/opt/homebrew/Cellar/libkrun")
}

func TestDescribeLoaded_NamesABundleProblem(t *testing.T) {
	r := describeLoaded([]string{"/x/libkrun.1.dylib", "/y/libkrunfw.5.dylib"}, nil,
		errors.New("libkrun resolved to /x/libkrun.1.dylib, not the copy shipped beside this daemon"))
	require.Len(t, r.Problems, 1)
	assert.Contains(t, r.Problems[0], "not the copy shipped beside this daemon")
}
