package libkrun

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/hyper-swe/mgit/internal/model"
	"github.com/hyper-swe/mgit/internal/sandboxd/backend/microvm"
)

// These tests pin the ENFORCEMENT half of the ADR-010 guardrail: not merely
// that the backend decides on a NIC (netbacking_test.go), but that every
// configured context carries one AND owns a live host peer for it, and that
// any failure destroys both rather than handing back something that would
// boot into a hang. Refs: FR-17.7, SEC-04, SEC-10, ADR-010

// fakeKrun records the libkrun call sequence so the configuration order and
// the fail-closed paths are testable without libkrun installed. failOn names
// the single call that should fail.
type fakeKrun struct {
	calls      []string
	failOn     string
	freeErr    error
	lastMAC    string
	lastNetSoc string
	lastVCPUs  uint8
	lastRAM    uint32
}

// record appends an op and fails it when it is the scripted failure point.
func (f *fakeKrun) record(op string) error {
	f.calls = append(f.calls, op)
	if op == f.failOn {
		return fmt.Errorf("krun_%s: -22", op)
	}
	return nil
}

func (f *fakeKrun) CreateCtx() (uint32, error) {
	if err := f.record("create_ctx"); err != nil {
		return 0, err
	}
	return 7, nil
}

func (f *fakeKrun) AddNetUnixgram(_ uint32, socketPath, mac string) error {
	f.lastNetSoc, f.lastMAC = socketPath, mac
	return f.record("add_net")
}

func (f *fakeKrun) SetVMConfig(_ uint32, vcpus uint8, ramMiB uint32) error {
	f.lastVCPUs, f.lastRAM = vcpus, ramMiB
	return f.record("set_vm_config")
}

func (f *fakeKrun) FreeCtx(uint32) error {
	_ = f.record("free_ctx")
	return f.freeErr
}

func (f *fakeKrun) seq() string { return strings.Join(f.calls, ",") }

func baseCfg(mode string) microvm.VMConfig {
	return microvm.VMConfig{
		SandboxID:   "sbx-abc123",
		NetworkMode: mode,
		CPUs:        2,
		MemoryMB:    1024,
	}
}

// denySocketExists reports whether the none-mode host peer is currently bound
// in dir — i.e. whether the context is holding a live host resource.
func denySocketExists(dir string) bool {
	_, err := os.Stat(filepath.Join(dir, denySocketName))
	return err == nil
}

func TestNewGuestCtx_AttachesTheNICBeforeAnythingElse(t *testing.T) {
	dir := shortTempDir(t)
	api := &fakeKrun{}

	gc, err := newGuestCtx(api, baseCfg(model.NetworkModeNone), dir, &stubAuthorizer{}, nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	t.Cleanup(func() { _ = gc.Close() })

	// The NIC must be attached immediately after the context exists, so there
	// is no window in which a live context lacks one.
	if want := "create_ctx,add_net,set_vm_config"; api.seq() != want {
		t.Errorf("call sequence = %q, want %q", api.seq(), want)
	}
	if want := filepath.Join(dir, denySocketName); api.lastNetSoc != want {
		t.Errorf("net backing socket = %q, want %q", api.lastNetSoc, want)
	}
	if api.lastMAC != microvm.GuestMAC(baseCfg(model.NetworkModeNone).SandboxID) {
		t.Errorf("MAC = %q, want the sandbox's derived address", api.lastMAC)
	}
	// The peer must be live, not merely named: an unserved socket hangs the VM.
	if !denySocketExists(dir) {
		t.Error("no host peer bound for the attached NIC")
	}
}

func TestNewGuestCtx_AnyConfigureStepFails_FailsClosedWithoutLeakingContextOrPeer(t *testing.T) {
	tests := []struct {
		name    string
		failOn  string
		wantSeq string // exact sequence: presence of free_ctx proves no leak
	}{
		{name: "create_fails", failOn: "create_ctx", wantSeq: "create_ctx"},
		{name: "net_attach_fails", failOn: "add_net", wantSeq: "create_ctx,add_net,free_ctx"},
		{name: "vm_config_fails", failOn: "set_vm_config", wantSeq: "create_ctx,add_net,set_vm_config,free_ctx"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dir := shortTempDir(t)
			api := &fakeKrun{failOn: tt.failOn}

			gc, err := newGuestCtx(api, baseCfg(model.NetworkModeNone), dir, &stubAuthorizer{}, nil)
			if err == nil {
				t.Fatal("expected an error: a half-configured context could boot on TSI")
			}
			if gc != nil {
				t.Error("a context was returned despite configuration failing")
			}
			if api.seq() != tt.wantSeq {
				t.Errorf("call sequence = %q, want %q", api.seq(), tt.wantSeq)
			}
			// The host peer is bound before the context exists, so every
			// failure path must unbind it (SEC-10: no residue).
			if denySocketExists(dir) {
				t.Error("host peer socket left bound after a failed launch")
			}
		})
	}
}

func TestNewGuestCtx_Close_ReleasesBothContextAndHostPeer(t *testing.T) {
	dir := shortTempDir(t)
	api := &fakeKrun{}

	gc, err := newGuestCtx(api, baseCfg(model.NetworkModeNone), dir, &stubAuthorizer{}, nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if err := gc.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if !strings.Contains(api.seq(), "free_ctx") {
		t.Errorf("libkrun context not freed: %q", api.seq())
	}
	if denySocketExists(dir) {
		t.Error("host peer socket still bound after Close")
	}
}

func TestNewGuestCtx_FreeAlsoFails_ReportsBothErrors(t *testing.T) {
	dir := shortTempDir(t)
	api := &fakeKrun{failOn: "add_net", freeErr: errors.New("krun_free_ctx: -1")}

	_, err := newGuestCtx(api, baseCfg(model.NetworkModeNone), dir, &stubAuthorizer{}, nil)
	if err == nil {
		t.Fatal("expected an error")
	}
	// Neither failure may be swallowed: the cause explains the launch, the
	// free failure explains the leaked host resource.
	for _, want := range []string{"attach net device", "free context"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q does not mention %q", err, want)
		}
	}
}

func TestNewGuestCtx_InvalidNetworkMode_NeverTouchesLibkrun(t *testing.T) {
	api := &fakeKrun{}
	if _, err := newGuestCtx(api, baseCfg("bogus-mode"), shortTempDir(t), &stubAuthorizer{}, nil); err == nil {
		t.Fatal("expected an error for an unknown network mode")
	}
	if len(api.calls) != 0 {
		t.Errorf("libkrun was touched before the mode was validated: %q", api.seq())
	}
}

func TestNewGuestCtx_ResourceCaps(t *testing.T) {
	def := model.DefaultSandboxPolicy()
	tests := []struct {
		name              string
		cpus, mb          int
		wantCPUs          uint8
		wantRAM           uint32
		wantDefaultsApply bool
	}{
		{name: "explicit", cpus: 4, mb: 2048, wantCPUs: 4, wantRAM: 2048},
		// 0 means "policy default" (model.SandboxPolicy), not "smallest
		// possible VM" — a 1 MiB guest would fail to boot inscrutably.
		{name: "unset_uses_policy_default", cpus: 0, mb: 0, wantDefaultsApply: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			api := &fakeKrun{}
			cfg := baseCfg(model.NetworkModeNone)
			cfg.CPUs, cfg.MemoryMB = tt.cpus, tt.mb

			gc, err := newGuestCtx(api, cfg, shortTempDir(t), &stubAuthorizer{}, nil)
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			t.Cleanup(func() { _ = gc.Close() })

			wantCPUs, wantRAM := tt.wantCPUs, tt.wantRAM
			if tt.wantDefaultsApply {
				wantCPUs, wantRAM = uint8(def.CPUs), uint32(def.MemoryMB) //nolint:gosec // OK: policy defaults are small constants
			}
			if api.lastVCPUs != wantCPUs {
				t.Errorf("vcpus = %d, want %d", api.lastVCPUs, wantCPUs)
			}
			if api.lastRAM != wantRAM {
				t.Errorf("ram = %d MiB, want %d", api.lastRAM, wantRAM)
			}
		})
	}
}

func TestVcpuCountAndMemoryMiB_ClampToTheCArgumentWidth(t *testing.T) {
	if got := vcpuCount(1 << 20); got != 255 {
		t.Errorf("vcpuCount(1<<20) = %d, want 255 (uint8 ceiling)", got)
	}
	if got := memoryMiB(1 << 40); got != 4294967295 {
		t.Errorf("memoryMiB(1<<40) = %d, want the uint32 ceiling", got)
	}
}
