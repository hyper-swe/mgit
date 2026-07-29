package libkrun

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/hyper-swe/mgit/internal/model"
)

// validSpec returns a spec that passes Validate: the guest root is a real
// directory (libkrun shares it over virtiofs; a file means someone handed us
// a disk image).
// testGuestBase builds a minimal but VALID guest root: the PID-1 supervisor
// plus the mount points it needs at boot (guestBaseDirs). Tests use it
// wherever they need a bootable base, so that "a root directory" in a test
// means the same thing it means in production — a tree that can actually
// boot. Tests of INVALID bases build their own broken trees.
func testGuestBase(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	for _, d := range append([]string{"sbin"}, guestBaseDirs...) {
		if err := os.MkdirAll(filepath.Join(root, d), 0o750); err != nil {
			t.Fatal(err)
		}
	}
	//nolint:gosec // G306: the guest PID-1 supervisor must be executable
	if err := os.WriteFile(filepath.Join(root, guestInitPath), []byte("#!/bin/true\n"), 0o750); err != nil {
		t.Fatal(err)
	}
	return root
}

func validSpec(t *testing.T) vmSpec {
	t.Helper()
	spec := baseSpec(model.NetworkModeNone, shortTempDir(t))
	spec.RootDir = testGuestBase(t)
	return spec
}

func TestVMSpec_Validate(t *testing.T) {
	imageFile := filepath.Join(t.TempDir(), "rootfs.img")
	if err := os.WriteFile(imageFile, []byte("ext4"), 0o600); err != nil {
		t.Fatal(err)
	}

	tests := []struct {
		name    string
		mutate  func(*vmSpec)
		wantErr string
	}{
		{name: "valid", mutate: func(*vmSpec) {}},
		{name: "missing_sandbox_id", mutate: func(s *vmSpec) { s.SandboxID = "" }, wantErr: "sandbox id"},
		{name: "missing_state_dir", mutate: func(s *vmSpec) { s.StateDir = "" }, wantErr: "state dir"},
		{name: "missing_root", mutate: func(s *vmSpec) { s.RootDir = "" }, wantErr: "root dir"},
		{name: "missing_exec", mutate: func(s *vmSpec) { s.ExecPath = "" }, wantErr: "exec path"},
		{name: "root_does_not_exist", mutate: func(s *vmSpec) { s.RootDir = "/definitely/not/here" }, wantErr: "guest root"},
		// A disk image is a firecracker/vzf artifact; libkrun boots a shared
		// DIRECTORY. Say so instead of failing inscrutably in the child.
		{name: "root_is_a_disk_image", mutate: func(s *vmSpec) { s.RootDir = imageFile }, wantErr: "not a directory"},
		{name: "unknown_network_mode", mutate: func(s *vmSpec) { s.NetworkMode = "bogus" }, wantErr: "unknown network mode"},
		{name: "state_dir_overflows_sun_path", mutate: func(s *vmSpec) {
			s.StateDir = "/" + strings.Repeat("d", maxUnixSocketPath)
		}, wantErr: "unix socket"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			spec := validSpec(t)
			tt.mutate(&spec)
			err := spec.Validate()
			if tt.wantErr == "" {
				if err != nil {
					t.Fatalf("Validate: %v", err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
				t.Fatalf("Validate = %v, want error mentioning %q", err, tt.wantErr)
			}
		})
	}
}

func TestVMSpec_Validate_VsockPathOverflow(t *testing.T) {
	spec := validSpec(t)
	spec.VsockEnabled = true
	// Long enough that the net backing socket fits but "vsock_1024.sock"
	// (longer name) does not: the check must cover EVERY socket the VM needs.
	spec.StateDir = "/" + strings.Repeat("d", maxUnixSocketPath-len("/"+denySocketName))
	err := spec.Validate()
	if err == nil || !errors.Is(err, model.ErrSandboxBackendUnavailable) {
		t.Fatalf("Validate = %v, want ErrSandboxBackendUnavailable for a vsock path over sun_path", err)
	}
}

func TestSpec_EncodeDecode_RoundTripsAndRevalidates(t *testing.T) {
	spec := validSpec(t)
	spec.TaskID = "MGIT-61.8"
	spec.WorktreePath = "/work/wt"
	spec.WorktreeTag = "work"
	spec.VsockEnabled = true
	spec.ExecArgs = []string{"--vsock-port", "1024"}
	spec.ExecEnv = []string{"PATH=/bin"}
	spec.Allowlist = []string{"example.com:443"}

	var buf bytes.Buffer
	if err := spec.encode(&buf); err != nil {
		t.Fatalf("encode: %v", err)
	}
	got, err := decodeSpec(&buf)
	if err != nil {
		t.Fatalf("decodeSpec: %v", err)
	}
	if got.SandboxID != spec.SandboxID || got.WorktreePath != spec.WorktreePath ||
		len(got.ExecArgs) != 2 || len(got.Allowlist) != 1 || !got.VsockEnabled {
		t.Errorf("round-trip lost fields: %+v", got)
	}
}

func TestDecodeSpec_RejectsGarbageAndInvalidSpecs(t *testing.T) {
	if _, err := decodeSpec(strings.NewReader("{ nope")); err == nil {
		t.Error("want a decode error for malformed JSON")
	}
	// Well-formed JSON, invalid spec: the child must re-validate rather than
	// trust the parent across the exec boundary.
	if _, err := decodeSpec(strings.NewReader(`{"sandbox_id":""}`)); err == nil {
		t.Error("want a validation error for an empty spec")
	}
}

// TestValidateGuestBase covers the host-side check that a guest tree can
// actually boot. It exists because of a real failure: a tree with only
// /sbin/mgit-guest booted and then died with
// "mount tmpfs overlay scratch /mnt: no such file or directory", which says
// nothing about the tree being incomplete.
//
// Under the old ext4 path the image builder created these directories, so
// nothing needed to check. libkrun shares a host DIRECTORY as the guest root,
// so any tree — ours, or one a user brings (MGIT-61.15) — can omit them.
// Refs: FR-17.3, MGIT-61.15
func TestValidateGuestBase(t *testing.T) {
	// completeBase builds a tree that satisfies the contract, then lets a
	// test break exactly one thing.
	completeBase := func(t *testing.T) string {
		t.Helper()
		root := t.TempDir()
		for _, d := range append([]string{"sbin"}, guestBaseDirs...) {
			if err := os.MkdirAll(filepath.Join(root, d), 0o750); err != nil {
				t.Fatal(err)
			}
		}
		//nolint:gosec // G306: the guest PID-1 supervisor must be executable
		if err := os.WriteFile(filepath.Join(root, "sbin", "mgit-guest"), []byte("#!/bin/true\n"), 0o750); err != nil {
			t.Fatal(err)
		}
		return root
	}

	tests := []struct {
		name    string
		breakIt func(t *testing.T, root string)
		wantErr string
	}{
		{name: "complete_base_passes"},
		{
			name:    "missing_supervisor",
			breakIt: func(t *testing.T, root string) { _ = os.Remove(filepath.Join(root, "sbin", "mgit-guest")) },
			wantErr: "must contain the mgit-guest supervisor",
		},
		{
			name: "supervisor_not_executable",
			breakIt: func(t *testing.T, root string) {
				if err := os.Chmod(filepath.Join(root, "sbin", "mgit-guest"), 0o600); err != nil {
					t.Fatal(err)
				}
			},
			wantErr: "not an executable file",
		},
		{
			// The exact failure that motivated this check.
			name:    "missing_overlay_scratch",
			breakIt: func(t *testing.T, root string) { _ = os.RemoveAll(filepath.Join(root, "mnt")) },
			wantErr: "/mnt",
		},
		{
			name: "missing_several_mount_points",
			breakIt: func(t *testing.T, root string) {
				_ = os.RemoveAll(filepath.Join(root, "proc"))
				_ = os.RemoveAll(filepath.Join(root, "tmp"))
			},
			wantErr: "/proc, /tmp",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			root := completeBase(t)
			if tt.breakIt != nil {
				tt.breakIt(t, root)
			}
			err := validateGuestBase(root, "/sbin/mgit-guest")
			if tt.wantErr == "" {
				if err != nil {
					t.Fatalf("a complete base must pass: %v", err)
				}
				return
			}
			if err == nil {
				t.Fatal("an unbootable base must be refused before a VM is spawned")
			}
			if !strings.Contains(err.Error(), tt.wantErr) {
				t.Errorf("error %q does not mention %q", err, tt.wantErr)
			}
			// The message must say how to fix it, not just what is wrong.
			if strings.Contains(tt.wantErr, "/") && !strings.Contains(err.Error(), "mkdir -p") {
				t.Errorf("error %q does not tell the user how to fix it", err)
			}
		})
	}
}
