// Package model defines pure domain types for mgit.
// These tests verify the SandboxManager and attestation interfaces per
// MGIT-11.2.3 acceptance criteria. Refs: FR-17.6, FR-17.12, FR-17.15
package model

import (
	"context"
	"encoding/json"
	"go/ast"
	"go/parser"
	"go/token"
	"reflect"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// stubSandboxManager is a compile-time proof that the interface is
// implementable with model types only (no third-party leakage).
type stubSandboxManager struct{}

func (stubSandboxManager) Launch(_ context.Context, _ SandboxLaunchOptions) (*SandboxInfo, error) {
	return &SandboxInfo{}, nil
}
func (stubSandboxManager) List(_ context.Context) ([]SandboxInfo, error) { return nil, nil }
func (stubSandboxManager) Exec(_ context.Context, _ string, _ ExecRequest) (*ExecResult, error) {
	return &ExecResult{}, nil
}
func (stubSandboxManager) Stop(_ context.Context, _ string, _ bool) error   { return nil }
func (stubSandboxManager) Remove(_ context.Context, _ string, _ bool) error { return nil }
func (stubSandboxManager) Resolve(_ context.Context, _ string) (*SandboxInfo, error) {
	return &SandboxInfo{}, nil
}

var _ SandboxManager = stubSandboxManager{}

// TestSandboxManager_InterfaceShape verifies the interface mirrors
// WorktreeManager's lifecycle verbs per FR-17.15 / ADR-005.
func TestSandboxManager_InterfaceShape(t *testing.T) {
	iface := reflect.TypeOf((*SandboxManager)(nil)).Elem()

	got := make([]string, 0, iface.NumMethod())
	for i := 0; i < iface.NumMethod(); i++ {
		got = append(got, iface.Method(i).Name)
	}
	sort.Strings(got)

	want := []string{"Exec", "Launch", "List", "Remove", "Resolve", "Stop"}
	assert.Equal(t, want, got, "SandboxManager must expose exactly the ADR-005 lifecycle verbs")

	t.Run("exec_result_carries_streams_and_exit", func(t *testing.T) {
		res := ExecResult{Stdout: []byte("ok"), Stderr: []byte("warn"), ExitCode: 3}
		data, err := json.Marshal(res)
		require.NoError(t, err)
		for _, key := range []string{`"stdout"`, `"stderr"`, `"exit_code"`} {
			assert.Contains(t, string(data), key)
		}
	})
}

// attestorGodoc parses sandbox.go and returns the doc comment attached
// to the Attestor type declaration (robust to formatting, unlike a raw
// substring scan).
func attestorGodoc(t *testing.T) string {
	t.Helper()
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, "sandbox.go", nil, parser.ParseComments)
	require.NoError(t, err)

	for _, decl := range file.Decls {
		gen, ok := decl.(*ast.GenDecl)
		if !ok || gen.Doc == nil {
			continue
		}
		for _, spec := range gen.Specs {
			if ts, ok := spec.(*ast.TypeSpec); ok && ts.Name.Name == "Attestor" {
				return gen.Doc.Text()
			}
		}
	}
	t.Fatal("Attestor type declaration with godoc not found in sandbox.go")
	return ""
}

// TestAttestor_HostSideContractDocumented verifies the SEC-01 contract
// is stated in the Attestor godoc: attestations are host-issued and
// guest code must never implement the interface or hold signing keys.
// This is a documentation-level audit control; the boundary itself is
// enforced by key isolation (FR-17.38).
func TestAttestor_HostSideContractDocumented(t *testing.T) {
	doc := strings.ToLower(attestorGodoc(t))

	for _, phrase := range []string{"host", "guest", "signing key", "sec-01"} {
		assert.Contains(t, doc, phrase,
			"Attestor godoc must state the host-side contract (%q)", phrase)
	}

	var _ = Attestation{} // the attestation payload type must exist
}

// TestCapabilityRequest_CarriesObservedDest verifies SEC-05: grant
// requests carry only host-observed destination facts, never
// guest-supplied remedy text.
func TestCapabilityRequest_CarriesObservedDest(t *testing.T) {
	req := CapabilityRequest{
		SandboxID:        "01JXEXAMPLEULID0000000000",
		TaskID:           "MGIT-4.2",
		Capability:       "egress",
		ObservedDestIP:   "140.82.112.22",
		ObservedDestPort: 22,
	}

	data, err := json.Marshal(req)
	require.NoError(t, err)
	for _, key := range []string{`"sandbox_id"`, `"task_id"`, `"capability"`, `"observed_dest_ip"`, `"observed_dest_port"`} {
		assert.Contains(t, string(data), key, "grant requests must carry observed-destination fields")
	}

	typ := reflect.TypeOf(req)
	for i := 0; i < typ.NumField(); i++ {
		name := strings.ToLower(typ.Field(i).Name)
		assert.NotContains(t, name, "remedy",
			"CapabilityRequest must not carry guest remedy text (SEC-05)")
		assert.NotContains(t, name, "message",
			"CapabilityRequest must not carry guest-supplied text (SEC-05)")
	}
}

// TestCapabilityRequest_Validate covers capability vocabulary and
// required fields. Refs: FR-17.12
func TestCapabilityRequest_Validate(t *testing.T) {
	tests := []struct {
		name       string
		sandboxID  string
		capability string
		wantErr    bool
	}{
		{name: "egress", sandboxID: "01JX", capability: CapabilityEgress, wantErr: false},
		{name: "ssh_agent", sandboxID: "01JX", capability: CapabilitySSHAgent, wantErr: false},
		{name: "open_network", sandboxID: "01JX", capability: CapabilityOpenNetwork, wantErr: false},
		{name: "mount", sandboxID: "01JX", capability: CapabilityMount, wantErr: false},
		{name: "unknown_capability", sandboxID: "01JX", capability: "allow_all", wantErr: true},
		{name: "empty_capability", sandboxID: "01JX", capability: "", wantErr: true},
		{name: "empty_sandbox_id", sandboxID: "", capability: CapabilityEgress, wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			req := CapabilityRequest{SandboxID: tt.sandboxID, TaskID: "MGIT-4.2", Capability: tt.capability}
			err := req.Validate()
			if tt.wantErr {
				assert.Error(t, err)
			} else {
				assert.NoError(t, err)
			}
		})
	}
}

// TestExecRequest_Validate covers the exec request error paths.
// Refs: FR-17.11
func TestExecRequest_Validate(t *testing.T) {
	tests := []struct {
		name    string
		req     ExecRequest
		wantErr bool
	}{
		{name: "valid", req: ExecRequest{Command: []string{"go", "test", "./..."}}, wantErr: false},
		{name: "valid_with_timeout", req: ExecRequest{Command: []string{"make"}, Timeout: time.Minute}, wantErr: false},
		{name: "empty_command", req: ExecRequest{}, wantErr: true},
		{name: "negative_timeout", req: ExecRequest{Command: []string{"make"}, Timeout: -time.Second}, wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := tt.req.Validate()
			if tt.wantErr {
				assert.Error(t, err)
			} else {
				assert.NoError(t, err)
			}
		})
	}
}
