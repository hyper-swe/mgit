package main

import (
	"context"
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// refusingConnect is a connectFunc that cannot reach a daemon.
func refusingConnect(err error) connectFunc {
	return func(context.Context) (sandboxClient, error) { return nil, err }
}

// The localhost probe must read the guest's NAME TABLE, never run a resolver:
// the property under test is "localhost resolves WITHOUT a DNS query", and a
// resolver that succeeded via DNS (egress open, a base with no /etc/hosts)
// would pass a check whose whole point is that DNS is unavailable. And a probe
// that could not run must be a reason (not-checked), never the MGIT-159
// failure: a base without the probe command has a name table nobody read.
// Refs: MGIT-169, MGIT-159
func TestProbeGuestLocalhost_ReadsTheNameTableNotAResolver(t *testing.T) {
	tests := []struct {
		name    string
		taskID  string
		connect connectFunc
		client  *fakeSandboxClient
		wantErr string
		wantOut string
	}{
		{
			name:    "no_bound_task_cannot_ask",
			connect: okConnect(&fakeSandboxClient{}),
			wantErr: "no sandbox",
		},
		{
			name:    "no_daemon_cannot_ask",
			taskID:  "T-1",
			connect: refusingConnect(errors.New("connection refused")),
			wantErr: "no sandbox daemon reachable",
		},
		{
			name:    "an_exec_failure_cannot_ask",
			taskID:  "T-1",
			client:  &fakeSandboxClient{execErr: errors.New("guest is not running")},
			wantErr: "guest is not running",
		},
		{
			name:    "a_table_that_maps_localhost_returns_that_entry",
			taskID:  "T-1",
			client:  &fakeSandboxClient{execStdout: "# static table\n127.0.0.1\tlocalhost\n::1\tlocalhost ip6-localhost\n"},
			wantOut: "127.0.0.1\tlocalhost",
		},
		{
			name:    "a_table_without_localhost_is_the_MGIT_159_condition",
			taskID:  "T-1",
			client:  &fakeSandboxClient{execStdout: "10.0.0.1\tgateway\n"},
			wantOut: "",
		},
		{
			name:    "a_table_that_only_MENTIONS_localhost_in_a_comment_is_the_condition_too",
			taskID:  "T-1",
			client:  &fakeSandboxClient{execStdout: "# localhost is handled by DNS\n"},
			wantOut: "",
		},
		{
			name:    "no_name_table_at_all_is_the_condition",
			taskID:  "T-1",
			client:  &fakeSandboxClient{execCode: 1, execStderr: "cat: /etc/hosts: No such file or directory\n"},
			wantOut: "",
		},
		{
			name:    "a_probe_command_the_guest_lacks_cannot_ask",
			taskID:  "T-1",
			client:  &fakeSandboxClient{execCode: 127, execStderr: "sh: cat: not found\n"},
			wantErr: "127",
		},
		{
			name:    "any_other_failure_to_read_the_table_cannot_ask",
			taskID:  "T-1",
			client:  &fakeSandboxClient{execCode: 1, execStderr: "cat: /etc/hosts: Permission denied\n"},
			wantErr: "Permission denied",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			connect := tt.connect
			if connect == nil {
				connect = okConnect(tt.client)
			}
			got, err := probeGuestLocalhost(context.Background(), connect, tt.taskID)

			if tt.wantErr != "" {
				require.Error(t, err, "a probe that could not run must say why, never answer")
				assert.Contains(t, err.Error(), tt.wantErr)
				return
			}
			require.NoError(t, err)
			assert.Equal(t, tt.wantOut, got)
			if tt.client != nil {
				assert.Equal(t, []string{"cat", "/etc/hosts"}, tt.client.execReq.Command,
					"the probe reads the name table; it must not run a resolver")
			}
		})
	}
}
