package sandboxd

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/hyper-swe/mgit/internal/controlproto"
)

// TestDaemon_Echo_AFullCapResponseArrivesIntactThroughTheSocket exercises the
// MGIT-160 property on the REAL channel: a running daemon, the real client,
// a unix socket, the greeting, the handshake, the deadlines — everything a
// `mgit doctor` run would use. The small size is the harness control (the
// harness can pass), the full cap is the property, and one byte over is the
// MGIT-160 contract: a legible refusal in the response, never an EOF.
// Refs: MGIT-175, MGIT-160
func TestDaemon_Echo_AFullCapResponseArrivesIntactThroughTheSocket(t *testing.T) {
	cfg, _ := testConfig(t, newFakeManager("01JXSB1"))
	cfg.Service = newDrainRecorder()
	cfg.IdleGrace = time.Hour
	ctx, cancel := context.WithCancel(context.Background())
	done := runDaemon(ctx, t, cfg)
	_ = waitForSocket(t, cfg.SocketPath)
	client := NewClient(cfg.SocketPath, time.Now)

	tests := []struct {
		name        string
		bytes       int
		wantIntact  bool
		wantRefusal string
	}{
		{"a_small_answer_is_the_harness_control", 4096, true, ""},
		{"the_full_cap_arrives_intact", controlproto.MaxResponseBytes, true, ""},
		{"one_byte_over_is_refused_legibly_not_as_EOF", controlproto.MaxResponseBytes + 1, false, "too large"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			out, err := client.Echo(ctx, tt.bytes)
			require.NoError(t, err, "the transport must carry the answer either way")
			if tt.wantIntact {
				require.NotNil(t, out.Result, "an answer was expected, got refusal %q", out.Refusal)
				assert.Empty(t, out.Refusal)
				assert.Equal(t, tt.bytes, out.Result.Bytes)
				assert.NoError(t, controlproto.VerifyEcho(out.Result), "the answer must arrive byte-intact")
				return
			}
			assert.Nil(t, out.Result, "an over-cap answer must not be delivered")
			assert.Contains(t, out.Refusal, tt.wantRefusal,
				"the daemon's refusal must reach the client as a response, not as a closed socket")
		})
	}

	cancel()
	require.NoError(t, <-done)
}
