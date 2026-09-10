package model

import (
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"
)

// The vocabulary is extended deliberately (FR-17.18): a guest that died is
// a state of its own, reached by one event, and both are valid to write.
func TestGuestDied_IsAStateBearingEventInTheClosedVocabulary(t *testing.T) {
	assert.True(t, ValidSandboxState(StateDead))
	assert.True(t, isValidEventType(EventGuestDied))
	state, ok := StateForEvent(EventGuestDied)
	assert.True(t, ok)
	assert.Equal(t, StateDead, state)
}

// The transport failures that only a reached-then-lost guest produces are
// one list, shared by the daemon (which marks the sandbox dead on them) and
// the CLI (which explains them): a marker in one and not the other would
// have the two disagree about the same failure. Refs: MGIT-99, MGIT-118
func TestIsLostServing(t *testing.T) {
	tests := []struct {
		name string
		err  error
		want bool
	}{
		{"stream_ended_mid_frame", errors.New("libkrun exec: guest exec: read frame: EOF"), true},
		{"peer_reset", errors.New("write: connection reset by peer"), true},
		{"broken_pipe", errors.New("write: broken pipe"), true},
		{"closed_conn", errors.New("use of closed network connection"), true},
		{"redial_refused", errors.New("guest vsock not ready within 15s: dial unix …: connect: connection refused"), true},
		{"a_read_deadline_is_the_hosts_patience_not_the_guest", errors.New("read: i/o timeout"), false},
		{"a_normal_nonzero_exit_is_not_a_loss", errors.New("exit status 2"), false},
		{"nil", nil, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, IsLostServing(tt.err))
		})
	}
}
