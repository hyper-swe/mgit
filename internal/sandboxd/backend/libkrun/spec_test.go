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
func validSpec(t *testing.T) vmSpec {
	t.Helper()
	spec := baseSpec(model.NetworkModeNone, shortTempDir(t))
	spec.RootDir = t.TempDir()
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
