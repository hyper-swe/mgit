// Package model defines pure domain types for mgit.
// These tests verify the FR-17 sandbox model types per MGIT-11.2.2
// acceptance criteria. Refs: FR-17.7, FR-17.15, FR-17.17, NFR-17.5
package model

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const testImageDigest = "go-node@sha256:6c3c624b58dbbcd3c0dd82b4c53f04194d1247c6eebdff7c1962cd129ab80d4f"

func validLaunchOptions() SandboxLaunchOptions {
	return SandboxLaunchOptions{
		TaskID:       "MGIT-4.2",
		WorktreePath: "/work/repos/mgit/worktrees/MGIT-4.2",
		ImageRef:     testImageDigest,
		Network:      NetworkPolicy{Mode: NetworkModeAllowlist, Allowlist: []string{"proxy.golang.org"}},
		CPUs:         2,
		MemoryMB:     2048,
		DiskQuotaMB:  4096,
		TTL:          4 * time.Hour,
	}
}

// TestNetworkPolicy_UnknownMode_Invalid verifies mode validation:
// only none|allowlist|open are accepted. Refs: FR-17.7
func TestNetworkPolicy_UnknownMode_Invalid(t *testing.T) {
	tests := []struct {
		name    string
		policy  NetworkPolicy
		wantErr bool
	}{
		{name: "mode_none", policy: NetworkPolicy{Mode: NetworkModeNone}, wantErr: false},
		{name: "mode_allowlist", policy: NetworkPolicy{Mode: NetworkModeAllowlist, Allowlist: []string{"pypi.org"}}, wantErr: false},
		{name: "mode_open", policy: NetworkPolicy{Mode: NetworkModeOpen}, wantErr: false},
		{name: "mode_empty", policy: NetworkPolicy{}, wantErr: true},
		{name: "mode_unknown", policy: NetworkPolicy{Mode: "bridge"}, wantErr: true},
		{name: "mode_case_sensitive", policy: NetworkPolicy{Mode: "OPEN"}, wantErr: true},
		{name: "allowlist_outside_allowlist_mode", policy: NetworkPolicy{Mode: NetworkModeNone, Allowlist: []string{"pypi.org"}}, wantErr: true},
		{name: "allowlist_wildcard_and_cidr_and_port", policy: NetworkPolicy{Mode: NetworkModeAllowlist, Allowlist: []string{"*.npmjs.org", "10.0.0.0/8", "staging.corp:22"}}, wantErr: false},
		{name: "allowlist_entry_control_char", policy: NetworkPolicy{Mode: NetworkModeAllowlist, Allowlist: []string{"evil\nFAKE AUDIT ROW"}}, wantErr: true},
		{name: "allowlist_entry_empty", policy: NetworkPolicy{Mode: NetworkModeAllowlist, Allowlist: []string{""}}, wantErr: true},
		{name: "allowlist_entry_uppercase", policy: NetworkPolicy{Mode: NetworkModeAllowlist, Allowlist: []string{"Registry.NPMJS.org"}}, wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := tt.policy.Validate()
			if tt.wantErr {
				require.Error(t, err)
				var vErr *ValidationError
				assert.ErrorAs(t, err, &vErr, "validation failures must be *ValidationError")
			} else {
				assert.NoError(t, err)
			}
		})
	}
}

// TestLaunchOptions_NonDigestImage_Invalid verifies image refs must be
// digest-pinned (FR-17.17): tag-only and malformed refs are rejected.
func TestLaunchOptions_NonDigestImage_Invalid(t *testing.T) {
	tests := []struct {
		name     string
		imageRef string
		wantErr  bool
	}{
		{name: "digest_pinned", imageRef: testImageDigest, wantErr: false},
		{name: "registry_with_port", imageRef: "registry.internal:5000/mgit/go-node@sha256:" + strings.Repeat("a", 64), wantErr: false},
		{name: "multi_component_path", imageRef: "ghcr.io/hyper-swe/go-node@sha256:" + strings.Repeat("b", 64), wantErr: false},
		{name: "tag_only", imageRef: "go-node:1.0", wantErr: true},
		{name: "tag_and_digest", imageRef: "go-node:1.22@sha256:" + strings.Repeat("a", 64), wantErr: true},
		{name: "no_reference", imageRef: "go-node", wantErr: true},
		{name: "empty", imageRef: "", wantErr: true},
		{name: "short_digest", imageRef: "go-node@sha256:abc123", wantErr: true},
		{name: "wrong_algorithm", imageRef: "go-node@md5:" + strings.Repeat("a", 64), wantErr: true},
		{name: "uppercase_hex", imageRef: "go-node@sha256:" + strings.Repeat("A", 64), wantErr: true},
		{name: "uppercase_name", imageRef: "go-Node@sha256:" + strings.Repeat("a", 64), wantErr: true},
		{name: "port_on_non_first_component", imageRef: "go/node:5000/x@sha256:" + strings.Repeat("a", 64), wantErr: true},
		{name: "oversized_ref", imageRef: strings.Repeat("a", 4096) + "@sha256:" + strings.Repeat("a", 64), wantErr: true},
		{name: "empty_path_component", imageRef: "a//b@sha256:" + strings.Repeat("a", 64), wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			opts := validLaunchOptions()
			opts.ImageRef = tt.imageRef
			err := opts.Validate()
			if tt.wantErr {
				var vErr *ValidationError
				require.Error(t, err)
				require.ErrorAs(t, err, &vErr)
				assert.Equal(t, "image_ref", vErr.Field)
			} else {
				assert.NoError(t, err)
			}
		})
	}
}

// TestLaunchOptions_EmptyTask_Invalid verifies task binding is
// mandatory and well-formed (FR-17.1).
func TestLaunchOptions_EmptyTask_Invalid(t *testing.T) {
	tests := []struct {
		name      string
		taskID    string
		wantField string
	}{
		{name: "empty_task", taskID: "", wantField: "task_id"},
		{name: "malformed_task", taskID: "not a task id!", wantField: "task_id"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			opts := validLaunchOptions()
			opts.TaskID = tt.taskID
			err := opts.Validate()
			var vErr *ValidationError
			require.Error(t, err)
			require.ErrorAs(t, err, &vErr)
			assert.Equal(t, tt.wantField, vErr.Field)
		})
	}

	t.Run("valid_options_pass", func(t *testing.T) {
		assert.NoError(t, validLaunchOptions().Validate())
	})

	t.Run("empty_worktree_path_invalid", func(t *testing.T) {
		opts := validLaunchOptions()
		opts.WorktreePath = ""
		var vErr *ValidationError
		err := opts.Validate()
		require.Error(t, err)
		require.ErrorAs(t, err, &vErr)
		assert.Equal(t, "worktree_path", vErr.Field)
	})

	t.Run("negative_resources_invalid", func(t *testing.T) {
		opts := validLaunchOptions()
		opts.CPUs = -1
		var vErr *ValidationError
		err := opts.Validate()
		require.Error(t, err)
		require.ErrorAs(t, err, &vErr)
		assert.Equal(t, "cpus", vErr.Field, "the error must name the offending field")
	})

	t.Run("zero_resources_mean_policy_default", func(t *testing.T) {
		opts := validLaunchOptions()
		opts.CPUs, opts.MemoryMB, opts.DiskQuotaMB, opts.TTL = 0, 0, 0, 0
		assert.NoError(t, opts.Validate())
	})

	t.Run("invalid_network_policy_propagates", func(t *testing.T) {
		opts := validLaunchOptions()
		opts.Network = NetworkPolicy{Mode: "bridge"}
		var vErr *ValidationError
		err := opts.Validate()
		require.Error(t, err)
		require.ErrorAs(t, err, &vErr)
		assert.Equal(t, "network.mode", vErr.Field,
			"nested errors must carry the parent field path")
	})

	t.Run("invalid_publish_ports_propagate", func(t *testing.T) {
		opts := validLaunchOptions()
		opts.PublishPorts = []PortPublish{{HostPort: 80, GuestPort: 3000}}
		var vErr *ValidationError
		err := opts.Validate()
		require.Error(t, err)
		require.ErrorAs(t, err, &vErr)
		assert.Equal(t, "publish_ports.host_port", vErr.Field,
			"nested publish-port errors must carry the parent field path")
	})

	t.Run("valid_publish_ports_pass", func(t *testing.T) {
		opts := validLaunchOptions()
		opts.PublishPorts = []PortPublish{{HostPort: 8080, GuestPort: 3000}, {HostPort: 9090, GuestPort: 5173}}
		assert.NoError(t, opts.Validate())
	})
}

// TestPortPublish_Validate verifies the one-way port-publish mapping
// validation: the host port binds loopback only and must be unprivileged,
// the guest port must be a valid TCP port, and all input is treated as
// hostile. Refs: SEC-09
func TestPortPublish_Validate(t *testing.T) {
	tests := []struct {
		name      string
		port      PortPublish
		wantErr   bool
		wantField string
	}{
		{name: "valid", port: PortPublish{HostPort: 8080, GuestPort: 3000}, wantErr: false},
		{name: "min_unprivileged_host_port", port: PortPublish{HostPort: 1024, GuestPort: 1}, wantErr: false},
		{name: "max_host_port", port: PortPublish{HostPort: 65535, GuestPort: 65535}, wantErr: false},
		{name: "privileged_host_port", port: PortPublish{HostPort: 80, GuestPort: 3000}, wantErr: true, wantField: "host_port"},
		{name: "privileged_host_port_1023", port: PortPublish{HostPort: 1023, GuestPort: 3000}, wantErr: true, wantField: "host_port"},
		{name: "zero_host_port", port: PortPublish{HostPort: 0, GuestPort: 3000}, wantErr: true, wantField: "host_port"},
		{name: "negative_host_port", port: PortPublish{HostPort: -1, GuestPort: 3000}, wantErr: true, wantField: "host_port"},
		{name: "overflow_host_port", port: PortPublish{HostPort: 70000, GuestPort: 3000}, wantErr: true, wantField: "host_port"},
		{name: "zero_guest_port", port: PortPublish{HostPort: 8080, GuestPort: 0}, wantErr: true, wantField: "guest_port"},
		{name: "negative_guest_port", port: PortPublish{HostPort: 8080, GuestPort: -1}, wantErr: true, wantField: "guest_port"},
		{name: "overflow_guest_port", port: PortPublish{HostPort: 8080, GuestPort: 70000}, wantErr: true, wantField: "guest_port"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := tt.port.Validate()
			if tt.wantErr {
				var vErr *ValidationError
				require.Error(t, err)
				require.ErrorAs(t, err, &vErr)
				assert.Equal(t, tt.wantField, vErr.Field)
			} else {
				assert.NoError(t, err)
			}
		})
	}
}

// TestSandboxLaunchOptions_PublishPorts_Duplicate verifies a host port
// published twice is rejected at the boundary (a duplicate bind would
// otherwise fail nondeterministically at listen time). Refs: SEC-09
func TestSandboxLaunchOptions_PublishPorts_Duplicate(t *testing.T) {
	opts := validLaunchOptions()
	opts.PublishPorts = []PortPublish{
		{HostPort: 8080, GuestPort: 3000},
		{HostPort: 8080, GuestPort: 5173},
	}
	var vErr *ValidationError
	err := opts.Validate()
	require.Error(t, err)
	require.ErrorAs(t, err, &vErr)
	assert.Equal(t, "publish_ports.host_port", vErr.Field)
	assert.Contains(t, vErr.Message, "duplicate")
}

// TestSandboxInfo_JSONRoundTrip_SnakeCase verifies SandboxInfo
// marshals with snake_case keys and round-trips losslessly.
// Refs: FR-17 (JSON tags), CODING-STYLE
func TestSandboxInfo_JSONRoundTrip_SnakeCase(t *testing.T) {
	info := SandboxInfo{
		ID:               "01JXEXAMPLEULID0000000000",
		TaskID:           "MGIT-4.2",
		WorktreePath:     "/work/repos/mgit/worktrees/MGIT-4.2",
		Backend:          BackendKVM,
		ImageDigest:      "sha256:6c3c624b58dbbcd3c0dd82b4c53f04194d1247c6eebdff7c1962cd129ab80d4f",
		NetworkMode:      NetworkModeAllowlist,
		NetworkAllowlist: []string{"proxy.golang.org"},
		State:            StateCreated,
		CreatedAt:        time.Date(2026, 6, 12, 12, 0, 0, 0, time.UTC),
		ExpiresAt:        time.Date(2026, 6, 12, 16, 0, 0, 0, time.UTC),
	}

	data, err := json.Marshal(info)
	require.NoError(t, err)

	wantKeys := []string{
		`"id"`, `"task_id"`, `"worktree_path"`, `"backend"`,
		`"image_digest"`, `"network_mode"`, `"network_allowlist"`,
		`"state"`, `"created_at"`, `"expires_at"`,
	}
	for _, key := range wantKeys {
		assert.Contains(t, string(data), key, "JSON must use snake_case key %s", key)
	}
	assert.Contains(t, string(data), "2026-06-12T12:00:00Z",
		"timestamps must serialize as ISO-8601 UTC")

	var got SandboxInfo
	require.NoError(t, json.Unmarshal(data, &got))
	assert.Equal(t, info, got, "round trip must be lossless")
}

// TestSandboxInfo_Validate_RequiredFields covers the error paths for
// the info type itself. Refs: FR-17.1
func TestSandboxInfo_Validate_RequiredFields(t *testing.T) {
	valid := SandboxInfo{
		ID: "01JX", TaskID: "MGIT-4.2", WorktreePath: "/w",
		Backend:     BackendKVM,
		ImageDigest: "sha256:" + strings.Repeat("c", 64),
		NetworkMode: NetworkModeNone,
	}
	assert.NoError(t, valid.Validate())

	tests := []struct {
		name   string
		mutate func(*SandboxInfo)
	}{
		{name: "empty_id", mutate: func(s *SandboxInfo) { s.ID = "" }},
		{name: "empty_task", mutate: func(s *SandboxInfo) { s.TaskID = "" }},
		{name: "malformed_task", mutate: func(s *SandboxInfo) { s.TaskID = "not a task!" }},
		{name: "empty_worktree_path", mutate: func(s *SandboxInfo) { s.WorktreePath = "" }},
		{name: "empty_image_digest", mutate: func(s *SandboxInfo) { s.ImageDigest = "" }},
		{name: "malformed_image_digest", mutate: func(s *SandboxInfo) { s.ImageDigest = "sha256:short" }},
		{name: "unknown_network_mode", mutate: func(s *SandboxInfo) { s.NetworkMode = "nat" }},
		{name: "unknown_backend", mutate: func(s *SandboxInfo) { s.Backend = "hyper-v" }},
		{name: "empty_backend", mutate: func(s *SandboxInfo) { s.Backend = "" }},
		{name: "unknown_state", mutate: func(s *SandboxInfo) { s.State = "destoyed" }},
		{name: "allowlist_in_none_mode", mutate: func(s *SandboxInfo) { s.NetworkAllowlist = []string{"x.io"} }},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			info := valid
			tt.mutate(&info)
			assert.Error(t, info.Validate())
		})
	}
}
