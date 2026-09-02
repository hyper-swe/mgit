package main

import (
	"context"
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/hyper-swe/mgit/internal/controlproto"
	"github.com/hyper-swe/mgit/internal/doctor"
	"github.com/hyper-swe/mgit/internal/sandboxd"
)

// Echo lets the in-memory client stand in for the daemon's echo verb.
func (f *fakeSandboxClient) Echo(_ context.Context, bytes int) (*sandboxd.EchoOutcome, error) {
	f.echoBytes = append(f.echoBytes, bytes)
	if f.echoErr != nil {
		return nil, f.echoErr
	}
	return &sandboxd.EchoOutcome{Result: f.echoResult, Refusal: f.echoRefusal}, nil
}

// echoless is a sandboxClient with no echo verb at all — the shape of a
// client that predates the check.
type echoless struct{ sandboxClient }

// probeResponseCap turns the client's echo into the doctor's reply: it asks
// for exactly the size it was given, verifies what came back, and reports the
// daemon's refusal verbatim when there was one. Every reason it cannot ask at
// all is an error, so the check reports not-checked rather than failed.
// Refs: MGIT-175, MGIT-160
func TestProbeResponseCap(t *testing.T) {
	full, err := controlproto.BuildEchoResponse(controlproto.MaxResponseBytes)
	require.NoError(t, err)
	tampered := *full.Echo
	tampered.Fill = "b" + tampered.Fill[1:]

	tests := []struct {
		name      string
		connect   connectFunc
		client    *fakeSandboxClient
		bytes     int
		wantErr   string
		wantReply doctor.EchoReply
	}{
		{
			name:    "no_daemon_reachable_cannot_ask",
			connect: func(context.Context) (sandboxClient, error) { return nil, errors.New("dial unix: no such file") },
			wantErr: "no sandbox daemon reachable",
		},
		{
			name:    "a_client_without_the_verb_cannot_ask",
			connect: okConnect(echoless{&fakeSandboxClient{}}),
			wantErr: "cannot ask the daemon to echo",
		},
		{
			name:    "a_transport_failure_is_returned_as_is",
			client:  &fakeSandboxClient{echoErr: errors.New("sandbox client: read response: EOF")},
			bytes:   controlproto.MaxResponseBytes + 1,
			wantErr: "read response: EOF",
		},
		{
			name:      "a_full_cap_answer_that_verifies_is_intact",
			client:    &fakeSandboxClient{echoResult: full.Echo},
			bytes:     controlproto.MaxResponseBytes,
			wantReply: doctor.EchoReply{Requested: controlproto.MaxResponseBytes, Intact: true},
		},
		{
			name:   "a_tampered_answer_is_not_intact_and_says_why",
			client: &fakeSandboxClient{echoResult: &tampered},
			bytes:  controlproto.MaxResponseBytes,
			wantReply: doctor.EchoReply{Requested: controlproto.MaxResponseBytes, Intact: false,
				Detail: "the answer did not verify: echo digest does not match its fill"},
		},
		{
			name:      "a_refusal_is_carried_verbatim",
			client:    &fakeSandboxClient{echoRefusal: "control response too large to send"},
			bytes:     controlproto.MaxResponseBytes + 1,
			wantReply: doctor.EchoReply{Requested: controlproto.MaxResponseBytes + 1, Refusal: "control response too large to send"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			connect := tt.connect
			if connect == nil {
				connect = okConnect(tt.client)
			}
			got, err := probeResponseCap(context.Background(), connect, tt.bytes)
			if tt.wantErr != "" {
				require.Error(t, err)
				assert.Contains(t, err.Error(), tt.wantErr)
				return
			}
			require.NoError(t, err)
			assert.Equal(t, tt.wantReply, got)
			assert.Equal(t, []int{tt.bytes}, tt.client.echoBytes, "the probe asks for exactly the size it was given")
		})
	}
}
