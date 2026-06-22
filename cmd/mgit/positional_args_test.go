package main

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

// TestFirstNonEmpty verifies the positional-vs-flag precedence helper used to
// standardize commit-hash / task-id arguments across commands: an explicit
// positional arg wins, otherwise the flag value is used. Refs: MGIT-23
func TestFirstNonEmpty(t *testing.T) {
	tests := []struct {
		name       string
		positional string
		flag       string
		want       string
	}{
		{name: "positional_only", positional: "deadbeef", flag: "", want: "deadbeef"},
		{name: "flag_only", positional: "", flag: "cafef00d", want: "cafef00d"},
		{name: "positional_wins", positional: "deadbeef", flag: "cafef00d", want: "deadbeef"},
		{name: "both_empty", positional: "", flag: "", want: ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, firstNonEmpty(tt.positional, tt.flag))
		})
	}
}

// TestArgAt verifies safe positional extraction. Refs: MGIT-23
func TestArgAt(t *testing.T) {
	assert.Equal(t, "x", argAt([]string{"x", "y"}, 0))
	assert.Equal(t, "y", argAt([]string{"x", "y"}, 1))
	assert.Equal(t, "", argAt([]string{"x"}, 1))
	assert.Equal(t, "", argAt(nil, 0))
}
