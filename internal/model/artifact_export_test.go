package model

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestArtifactExportRequest_Validate covers the boundary gate every export
// crosses before it reaches a backend. The authoritative containment checks run
// host-side in the export engine; this one exists so malformed input never gets
// that far. Refs: MGIT-73
func TestArtifactExportRequest_Validate(t *testing.T) {
	tests := []struct {
		name    string
		req     ArtifactExportRequest
		wantErr bool
		field   string
	}{
		{name: "valid", req: ArtifactExportRequest{GuestPath: "node_modules", HostPath: "/host/cache/nm"}},
		{name: "valid_nested", req: ArtifactExportRequest{GuestPath: "build/out", HostPath: "/host/x"}},
		{name: "empty_guest_path", req: ArtifactExportRequest{HostPath: "/host/x"}, wantErr: true, field: "guest_path"},
		{name: "blank_guest_path", req: ArtifactExportRequest{GuestPath: "   ", HostPath: "/host/x"}, wantErr: true, field: "guest_path"},
		{name: "absolute_guest_path", req: ArtifactExportRequest{GuestPath: "/etc/passwd", HostPath: "/host/x"}, wantErr: true, field: "guest_path"},
		{name: "nul_in_guest_path", req: ArtifactExportRequest{GuestPath: "a\x00b", HostPath: "/host/x"}, wantErr: true, field: "guest_path"},
		{name: "empty_host_path", req: ArtifactExportRequest{GuestPath: "out"}, wantErr: true, field: "host_path"},
		{name: "relative_host_path", req: ArtifactExportRequest{GuestPath: "out", HostPath: "rel"}, wantErr: true, field: "host_path"},
		{name: "nul_in_host_path", req: ArtifactExportRequest{GuestPath: "out", HostPath: "/host/a\x00b"}, wantErr: true, field: "host_path"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := tt.req.Validate()
			if !tt.wantErr {
				require.NoError(t, err)
				return
			}
			require.Error(t, err)
			var verr *ValidationError
			require.ErrorAs(t, err, &verr)
			assert.Equal(t, tt.field, verr.Field, "the refusal must name the offending field")
		})
	}
}

// TestArtifactExportedEvent_IsAuditOnly proves the export event is in the closed
// vocabulary AND carries no lifecycle state change: an export records that files
// crossed the boundary, it does not move the sandbox's state. Refs: MGIT-73, FR-17.18
func TestArtifactExportedEvent_IsAuditOnly(t *testing.T) {
	ev := SandboxEvent{SandboxID: "01JSB", TaskID: "MGIT-73", EventType: EventArtifactExported}
	require.NoError(t, ev.Validate(), "artifact_exported must be a valid event type")

	_, stateBearing := StateForEvent(EventArtifactExported)
	assert.False(t, stateBearing, "an export must not change the derived sandbox state")
	assert.Contains(t, NonStateEventTypes(), EventArtifactExported,
		"state derivation must skip past export records to the latest lifecycle event")
}
