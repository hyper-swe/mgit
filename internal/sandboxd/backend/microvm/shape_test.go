package microvm

import (
	"context"
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/hyper-swe/mgit/internal/model"
)

// Each backend boots exactly one shape of guest base: firecracker and vzf a
// kernel plus an ext4 rootfs image, libkrun a directory. The released v0.6.8
// on Linux linked firecracker, `sandbox base from` registered a directory,
// and every exec failed inside the VMM's own config check with `failed to
// stat kernel image path, ""`, which the CLI filed under "could not identify
// what failed". The cause was knowable before anything was created: the
// resolved image has no kernel. So Launch refuses the mismatch first, naming
// the backend, the base's shape and the provisioning that fits, and creates
// no VM and no state. Refs: MGIT-233
func TestManager_Launch_RefusesABaseItsBackendCannotBoot(t *testing.T) {
	dir := t.TempDir()
	root := filepath.Join(dir, "base-tree")
	require.NoError(t, os.MkdirAll(root, 0o750))
	kernel := filepath.Join(dir, "vmlinux")
	rootfs := filepath.Join(dir, "rootfs.ext4")
	require.NoError(t, os.WriteFile(kernel, []byte("kernel"), 0o600))
	require.NoError(t, os.WriteFile(rootfs, []byte("rootfs"), 0o600))
	directory := ImagePaths{RootfsPath: root}
	image := ImagePaths{KernelPath: kernel, RootfsPath: rootfs}
	noKernel := ImagePaths{RootfsPath: rootfs}

	tests := []struct {
		name    string
		backend string
		images  ImagePaths
		want    []string // nil: the launch proceeds
	}{
		{"firecracker_with_a_directory", model.BackendKVM, directory,
			[]string{"firecracker", "kernel + ext4 rootfs image", "is a directory", root, "mgit sandbox image install --from", "libkrun"}},
		{"firecracker_with_no_kernel", model.BackendKVM, noKernel,
			[]string{"firecracker", "no kernel", "mgit sandbox image install --from"}},
		{"vzf_with_a_directory", model.BackendVZF, directory,
			[]string{"vzf", "kernel + ext4 rootfs image", "is a directory", "mgit sandbox image install --from"}},
		{"libkrun_with_an_image", model.BackendLibkrun, image,
			[]string{"libkrun", "boots a directory", "kernel + rootfs image", rootfs, "mgit sandbox base from"}},
		{"firecracker_with_an_image", model.BackendKVM, image, nil},
		{"vzf_with_an_image", model.BackendVZF, image, nil},
		{"libkrun_with_a_directory", model.BackendLibkrun, directory, nil},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			hv := &fakeHypervisor{}
			workDir := t.TempDir()
			mgr, err := NewManager(Config{
				Backend:    tt.backend,
				WorkDir:    workDir,
				Resolve:    func(string) (ImagePaths, error) { return tt.images, nil },
				Hypervisor: hv,
				Logger:     slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError})),
				Clock:      func() time.Time { return time.Date(2026, 9, 24, 12, 0, 0, 0, time.UTC) },
			})
			require.NoError(t, err)
			_, err = mgr.Launch(context.Background(), launchOpts("T-1", model.NetworkModeNone))
			if tt.want == nil {
				assert.False(t, errors.Is(err, model.ErrGuestBaseUnbootable), "a base its backend boots is not refused: %v", err)
				assert.Len(t, hv.configs, 1, "the launch reached the hypervisor")
				return
			}
			require.ErrorIs(t, err, model.ErrGuestBaseUnbootable)
			for _, w := range tt.want {
				assert.Contains(t, err.Error(), w)
			}
			assert.Empty(t, hv.configs, "refused before any VM is created")
			entries, err := os.ReadDir(workDir)
			require.NoError(t, err)
			assert.Empty(t, entries, "and before any sandbox state is made")
		})
	}
}
