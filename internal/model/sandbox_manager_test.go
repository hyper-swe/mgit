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
		if !ok {
			continue
		}
		for _, spec := range gen.Specs {
			ts, ok := spec.(*ast.TypeSpec)
			if !ok || ts.Name.Name != "Attestor" {
				continue
			}
			// Doc may attach to the decl or, in grouped type blocks,
			// to the spec itself.
			if gen.Doc != nil {
				return gen.Doc.Text()
			}
			if ts.Doc != nil {
				return ts.Doc.Text()
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

// TestCapabilityRequest_Validate covers capability vocabulary,
// required identification, and the SEC-05 observed-destination
// requirement for egress. Refs: FR-17.12
func TestCapabilityRequest_Validate(t *testing.T) {
	egress := CapabilityRequest{
		SandboxID: "01JX", TaskID: "MGIT-4.2", Capability: CapabilityEgress,
		ObservedDestIP: "140.82.112.22", ObservedDestPort: 22,
	}
	plain := func(capability string) CapabilityRequest {
		return CapabilityRequest{SandboxID: "01JX", TaskID: "MGIT-4.2", Capability: capability}
	}
	mutate := func(base CapabilityRequest, fn func(*CapabilityRequest)) CapabilityRequest {
		fn(&base)
		return base
	}

	tests := []struct {
		name    string
		req     CapabilityRequest
		wantErr bool
	}{
		{name: "egress_with_observed_dest", req: egress, wantErr: false},
		{name: "ssh_agent", req: plain(CapabilitySSHAgent), wantErr: false},
		{name: "open_network", req: plain(CapabilityOpenNetwork), wantErr: false},
		{name: "mount", req: plain(CapabilityMount), wantErr: false},
		{name: "egress_missing_observed_ip", req: plain(CapabilityEgress), wantErr: true},
		{name: "egress_hostname_not_ip", req: mutate(egress, func(r *CapabilityRequest) { r.ObservedDestIP = "github.com" }), wantErr: true},
		{name: "egress_injection_string", req: mutate(egress, func(r *CapabilityRequest) { r.ObservedDestIP = "140.82.112.22<script>" }), wantErr: true},
		{name: "egress_ipv6_ok", req: mutate(egress, func(r *CapabilityRequest) { r.ObservedDestIP = "2606:50c0:8000::153" }), wantErr: false},
		{name: "egress_invalid_port", req: mutate(egress, func(r *CapabilityRequest) { r.ObservedDestPort = 0 }), wantErr: true},
		{name: "egress_port_too_large", req: mutate(egress, func(r *CapabilityRequest) { r.ObservedDestPort = 70000 }), wantErr: true},
		{name: "unknown_capability", req: plain("allow_all"), wantErr: true},
		{name: "empty_capability", req: plain(""), wantErr: true},
		{name: "empty_sandbox_id", req: mutate(egress, func(r *CapabilityRequest) { r.SandboxID = "" }), wantErr: true},
		{name: "empty_task_id", req: mutate(egress, func(r *CapabilityRequest) { r.TaskID = "" }), wantErr: true},
		{name: "malformed_task_id", req: mutate(egress, func(r *CapabilityRequest) { r.TaskID = "not a task!" }), wantErr: true},
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

// TestAttestation_Validate_Shape rejects structurally hollow
// attestations before they reach a verifier. Refs: FR-17.6
func TestAttestation_Validate_Shape(t *testing.T) {
	valid := Attestation{
		SandboxID:     "01JX",
		CommitHash:    strings.Repeat("a", 40),
		ContentHash:   strings.Repeat("b", 64),
		Alg:           AlgEd25519,
		KeyID:         "sha256:" + strings.Repeat("c", 64),
		HostSignature: []byte{0x01},
		IssuedAt:      time.Date(2026, 6, 12, 12, 0, 0, 0, time.UTC),
	}
	assert.NoError(t, valid.Validate())

	tests := []struct {
		name   string
		mutate func(*Attestation)
	}{
		{name: "empty_sandbox_id", mutate: func(a *Attestation) { a.SandboxID = "" }},
		{name: "short_commit_hash", mutate: func(a *Attestation) { a.CommitHash = "abc" }},
		{name: "short_content_hash", mutate: func(a *Attestation) { a.ContentHash = "abc" }},
		{name: "missing_alg", mutate: func(a *Attestation) { a.Alg = "" }},
		{name: "missing_key_id", mutate: func(a *Attestation) { a.KeyID = "" }},
		{name: "empty_signature", mutate: func(a *Attestation) { a.HostSignature = nil }},
		{name: "zero_issued_at", mutate: func(a *Attestation) { a.IssuedAt = time.Time{} }},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			att := valid
			tt.mutate(&att)
			assert.Error(t, att.Validate())
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
