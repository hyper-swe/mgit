package doctor

import (
	"context"
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/hyper-swe/mgit/internal/controlproto"
)

// answering returns a probe that answers every size at or under the cap
// intact and refuses every size over it legibly — the healthy daemon.
func answering() func(context.Context, int) (EchoReply, error) {
	return func(_ context.Context, n int) (EchoReply, error) {
		if n > controlproto.MaxResponseBytes {
			return EchoReply{Requested: n, Refusal: "control response too large to send: this verb's answer is 1048577 bytes"}, nil
		}
		return EchoReply{Requested: n, Intact: true}, nil
	}
}

// The response-cap check: ok only when a full-cap answer arrives intact AND
// an oversized one is refused legibly; not-checked when no daemon could be
// asked; failed, naming what was observed, for every other shape — including
// the shape MGIT-160 was, an oversized answer dying as a closed socket.
// Refs: MGIT-175, MGIT-160
func TestResponseCapCheck(t *testing.T) {
	tests := []struct {
		name       string
		probe      func(context.Context, int) (EchoReply, error)
		wantStatus Status
		wantIn     []string
	}{
		{
			name:       "a_full_cap_answer_and_a_legible_refusal_pass",
			probe:      answering(),
			wantStatus: StatusOK,
			wantIn:     []string{"1048576", "refused"},
		},
		{
			name: "no_daemon_is_not_checked_never_a_pass",
			probe: func(context.Context, int) (EchoReply, error) {
				return EchoReply{}, errors.New("no sandbox daemon reachable: dial unix: no such file")
			},
			wantStatus: StatusNotChecked,
			wantIn:     []string{"no sandbox daemon reachable"},
		},
		{
			name: "a_full_cap_answer_that_did_not_arrive_intact_fails_and_says_how",
			probe: func(_ context.Context, n int) (EchoReply, error) {
				if n == controlproto.MaxResponseBytes {
					return EchoReply{Requested: n, Detail: "digest mismatch after 1048576 bytes"}, nil
				}
				return answering()(context.Background(), n)
			},
			wantStatus: StatusFailed,
			wantIn:     []string{"did not arrive intact", "digest mismatch"},
		},
		{
			name: "a_full_cap_answer_the_daemon_refused_fails_and_quotes_the_refusal",
			probe: func(_ context.Context, n int) (EchoReply, error) {
				if n == controlproto.MaxResponseBytes {
					return EchoReply{Requested: n, Refusal: "sandbox: invalid request"}, nil
				}
				return answering()(context.Background(), n)
			},
			wantStatus: StatusFailed,
			wantIn:     []string{"refused", "invalid request"},
		},
		{
			name: "an_oversized_answer_that_died_as_EOF_is_the_MGIT-160_failure",
			probe: func(_ context.Context, n int) (EchoReply, error) {
				if n > controlproto.MaxResponseBytes {
					return EchoReply{}, errors.New("sandbox client: read response: EOF")
				}
				return answering()(context.Background(), n)
			},
			wantStatus: StatusFailed,
			wantIn:     []string{"EOF", "instead of a refusal"},
		},
		{
			name: "an_oversized_answer_that_was_delivered_means_the_cap_is_not_enforced",
			probe: func(_ context.Context, n int) (EchoReply, error) {
				return EchoReply{Requested: n, Intact: true}, nil
			},
			wantStatus: StatusFailed,
			wantIn:     []string{"not enforced"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := ResponseCapCheck{Probe: tt.probe}.Run(context.Background())

			assert.Equal(t, "daemon/response-cap", got.Name)
			assert.Equal(t, "MGIT-160", got.Incident)
			require.Equal(t, tt.wantStatus, got.Status, "summary: %s reason: %s", got.Summary, got.Reason)
			text := got.Summary + " " + got.Reason + " " + got.Remedy
			for _, want := range tt.wantIn {
				assert.Contains(t, text, want)
			}
			if got.Status == StatusFailed {
				assert.NotEmpty(t, got.Remedy, "a failure without a next step moves the mystery instead of removing it")
			}
		})
	}
}
