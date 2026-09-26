package libkrun

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/hyper-swe/mgit/internal/model"
)

// A base the libkrun backend cannot boot (no supervisor, a supervisor that
// is not executable, missing mount points) was refused as "no sandbox
// backend available on this platform", which sends the reader to install a
// hypervisor that is already there. It is a base this backend cannot boot,
// and says so with the sentinel the CLI names without disowning it.
// Refs: MGIT-233.1, MGIT-233, MGIT-232
func TestValidateGuestBase_AnUnbootableBaseIsNotAMissingBackend(t *testing.T) {
	tests := []struct {
		name    string
		breakIt func(t *testing.T, root string)
	}{
		{"no_supervisor", func(t *testing.T, root string) {
			if err := os.Remove(filepath.Join(root, "sbin", "mgit-guest")); err != nil {
				t.Fatal(err)
			}
		}},
		{"supervisor_not_executable", func(t *testing.T, root string) {
			if err := os.Chmod(filepath.Join(root, "sbin", "mgit-guest"), 0o600); err != nil {
				t.Fatal(err)
			}
		}},
		{"missing_mount_points", func(t *testing.T, root string) {
			if err := os.RemoveAll(filepath.Join(root, "proc")); err != nil {
				t.Fatal(err)
			}
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			root := testGuestBase(t)
			tt.breakIt(t, root)
			err := validateGuestBase(root, guestInitPath)
			if !errors.Is(err, model.ErrGuestBaseUnbootable) {
				t.Fatalf("want ErrGuestBaseUnbootable, got %v", err)
			}
			if errors.Is(err, model.ErrSandboxBackendUnavailable) || strings.Contains(err.Error(), "no sandbox backend available") {
				t.Fatalf("the backend is available; the base is not bootable: %v", err)
			}
		})
	}
}
