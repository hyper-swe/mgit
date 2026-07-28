package libkrun

import (
	"errors"
	"path/filepath"
	"strings"
	"testing"

	"github.com/hyper-swe/mgit/internal/model"
	"github.com/hyper-swe/mgit/internal/sandboxd/backend/microvm"
)

// The libkrun backend's single most important invariant: EVERY launch
// configures an explicit host-backed NIC. libkrun auto-enables TSI
// (Transparent Socket Impersonation) when no network device is added, which
// proxies the guest's sockets through the host — i.e. a config that omits the
// NIC is a silent, full egress leak, not a closed network. These tests pin
// that invariant for every mode, including "none". Refs: FR-17.7, SEC-04, ADR-010

const testStateDir = "/state/sbx"

func TestNetBackingFor_EveryMode_AttachesExplicitNIC(t *testing.T) {
	tests := []struct {
		name       string
		mode       string
		wantSocket string
		wantDeny   bool
	}{
		// "none" is the case that would leak if the backend honored
		// VMConfig.AttachNIC=false the way vzf and firecracker safely can.
		{name: "none", mode: model.NetworkModeNone, wantSocket: denySocketName, wantDeny: true},
		{name: "allowlist", mode: model.NetworkModeAllowlist, wantSocket: proxySocketName},
		{name: "open", mode: model.NetworkModeOpen, wantSocket: proxySocketName},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := netBackingFor("sbx-abc123", tt.mode, testStateDir)
			if err != nil {
				t.Fatalf("netBackingFor(%q) unexpected error: %v", tt.mode, err)
			}
			if got.SocketPath == "" {
				t.Fatalf("mode %q produced NO explicit net device: libkrun would fall "+
					"back to TSI and leak host egress", tt.mode)
			}
			if want := filepath.Join(testStateDir, tt.wantSocket); got.SocketPath != want {
				t.Errorf("SocketPath = %q, want %q", got.SocketPath, want)
			}
			if got.Deny() != tt.wantDeny {
				t.Errorf("Deny() = %v, want %v", got.Deny(), tt.wantDeny)
			}
			if got.Mode != tt.mode {
				t.Errorf("Mode = %q, want %q", got.Mode, tt.mode)
			}
			if got.MAC == "" {
				t.Error("MAC is empty; libkrun requires an explicit MAC for the NIC")
			}
		})
	}
}

func TestNetBackingFor_InvalidInput_FailsClosed(t *testing.T) {
	tests := []struct {
		name     string
		mode     string
		stateDir string
	}{
		{name: "empty_state_dir_has_nowhere_for_the_socket", mode: model.NetworkModeNone, stateDir: ""},
		{name: "unknown_mode", mode: "bogus-mode", stateDir: testStateDir},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := netBackingFor("sbx-abc123", tt.mode, tt.stateDir)
			if err == nil {
				t.Fatal("expected an error: booting anyway would fall back to TSI")
			}
			if !errors.Is(err, model.ErrSandboxBackendUnavailable) {
				t.Errorf("error = %v, want it to wrap ErrSandboxBackendUnavailable", err)
			}
		})
	}
}

// The MAC derivation itself is covered by microvm.GuestMAC's own test; this
// pins only that a backing carries the sandbox's address rather than a
// constant shared across sandboxes.
func TestNetBackingFor_MACIsPerSandbox(t *testing.T) {
	a, err := netBackingFor("sbx-abc123", model.NetworkModeNone, testStateDir)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	b, err := netBackingFor("sbx-different", model.NetworkModeNone, testStateDir)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if a.MAC == b.MAC {
		t.Errorf("distinct sandboxes share a MAC (%q)", a.MAC)
	}
	if want := microvm.GuestMAC("sbx-abc123"); a.MAC != want {
		t.Errorf("MAC = %q, want the shared derivation %q", a.MAC, want)
	}
}

func TestParseMAC_RoundTripsTheDerivedAddress(t *testing.T) {
	tests := []struct {
		name    string
		mac     string
		want    [6]byte
		wantErr bool
	}{
		{name: "derived_form", mac: "02:aa:bb:cc:dd:ee", want: [6]byte{0x02, 0xaa, 0xbb, 0xcc, 0xdd, 0xee}},
		{name: "uppercase", mac: "02:AA:BB:CC:DD:EE", want: [6]byte{0x02, 0xaa, 0xbb, 0xcc, 0xdd, 0xee}},
		{name: "too_few_octets", mac: "02:aa:bb:cc:dd", wantErr: true},
		{name: "not_hex", mac: "02:aa:bb:cc:dd:zz", wantErr: true},
		// net.ParseMAC also accepts EUI-64; libkrun's device takes six bytes.
		{name: "eui64_is_too_wide", mac: "02:aa:bb:cc:dd:ee:ff:00", wantErr: true},
		{name: "empty", mac: "", wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := parseMAC(tt.mac)
			if tt.wantErr {
				if err == nil {
					t.Fatalf("parseMAC(%q) = %v, want an error", tt.mac, got)
				}
				return
			}
			if err != nil {
				t.Fatalf("parseMAC(%q) unexpected error: %v", tt.mac, err)
			}
			if got != tt.want {
				t.Errorf("parseMAC(%q) = %v, want %v", tt.mac, got, tt.want)
			}
		})
	}
}

// The MACs the backend actually generates must parse — the derivation and the
// parser must not drift apart.
func TestParseMAC_AcceptsEveryGeneratedMAC(t *testing.T) {
	for _, id := range []string{"sbx-abc123", "sbx-different", "01JABCDEF0123456789KLMNOPQ", ""} {
		backing, err := netBackingFor(id, "none", testStateDir)
		if err != nil {
			t.Fatalf("netBackingFor(%q): %v", id, err)
		}
		if _, err := parseMAC(backing.MAC); err != nil {
			t.Errorf("generated MAC %q for sandbox %q does not parse: %v", backing.MAC, id, err)
		}
	}
}

func TestNetBackingFor_OverlongSocketPath_ReportsTheRealReason(t *testing.T) {
	// bind(2) would say only "invalid argument"; the operator needs the cause,
	// and both the deny and proxy socket paths must be covered.
	long := "/" + strings.Repeat("d", 120)
	for _, mode := range []string{model.NetworkModeNone, model.NetworkModeAllowlist} {
		_, err := netBackingFor("sbx-abc123", mode, long)
		if err == nil {
			t.Fatalf("mode %q: expected an error for a path over the unix socket limit", mode)
		}
		if !strings.Contains(err.Error(), "unix socket limit") {
			t.Errorf("mode %q: error %q does not explain the path-length limit", mode, err)
		}
	}
}
