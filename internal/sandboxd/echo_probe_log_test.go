package sandboxd

import (
	"context"
	"encoding/json"
	"io"
	"net"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/hyper-swe/mgit/internal/controlproto"
)

// logRecords parses the daemon's JSON log into records.
func logRecords(t *testing.T, raw string) []map[string]any {
	t.Helper()
	var out []map[string]any
	for _, line := range strings.Split(strings.TrimSpace(raw), "\n") {
		if line == "" {
			continue
		}
		var rec map[string]any
		require.NoError(t, json.Unmarshal([]byte(line), &rec), "log line %q", line)
		out = append(out, rec)
	}
	return out
}

// Every `mgit doctor` left two WARN "sandboxd write response failed" lines in
// daemon.log, event write_error, advising the reader to exclude node_modules.
// That is daemon/response-cap proving the MGIT-160 refusal ON PURPOSE (doctor
// reports it ok), logged in the words of a real failure, and it is the first
// thing a reader finds in the log after a real one. The echo verb exists only
// for that probe, so its over-size refusal is logged as the probe it is:
// its own event, below WARN, naming doctor. The refusal still reaches the
// client as a response. Refs: MGIT-235, MGIT-160
func TestDaemon_ResponseCapProbe_IsLoggedAsAProbeNotAsAWriteFailure(t *testing.T) {
	cfg, logs := testConfig(t, newFakeManager("01JXSB1"))
	cfg.Service = newDrainRecorder()
	cfg.IdleGrace = time.Hour
	ctx, cancel := context.WithCancel(context.Background())
	done := runDaemon(ctx, t, cfg)
	_ = waitForSocket(t, cfg.SocketPath)

	out, err := NewClient(cfg.SocketPath, time.Now).Echo(ctx, controlproto.MaxResponseBytes+1)
	require.NoError(t, err)
	assert.Contains(t, out.Refusal, "too large", "the probe's refusal still arrives as a response")
	cancel()
	require.NoError(t, <-done)

	var probe map[string]any
	for _, rec := range logRecords(t, logs.String()) {
		assert.NotEqual(t, "write_error", rec["event"], "the probe is not a write failure: %v", rec)
		if rec["event"] == "response_cap_probe" {
			probe = rec
		}
	}
	require.NotNil(t, probe, "the probe is logged as itself:\n%s", logs.String())
	assert.Equal(t, "INFO", probe["level"], "below WARN: nothing failed")
	assert.Contains(t, probe["msg"], "doctor", "and it names who asked for it")
}

// A real over-size response is still a WARN write_error: only the probe verb
// is exempt, not the refusal path. Refs: MGIT-235, MGIT-160
func TestDaemon_ARealOversizeResponse_IsStillLoggedAsAWriteFailure(t *testing.T) {
	cfg, logs := testConfig(t, newFakeManager("01JXSB1"))
	d, err := New(cfg)
	require.NoError(t, err)
	near, far := net.Pipe()
	defer func() { _ = near.Close() }()
	go func() { _, _ = io.Copy(io.Discard, far) }()

	d.writeResponse(near, &controlproto.Response{Error: strings.Repeat("x", controlproto.MaxResponseBytes+16)})

	var warned bool
	for _, rec := range logRecords(t, logs.String()) {
		if rec["event"] == "write_error" && rec["level"] == "WARN" {
			warned = true
		}
		assert.NotEqual(t, "response_cap_probe", rec["event"], "a real over-size answer is not the probe")
	}
	assert.True(t, warned, "a real over-size response is a WARN write_error:\n%s", logs.String())
}
