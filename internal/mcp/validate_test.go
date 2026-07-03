package mcp

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestValidateTaskID(t *testing.T) {
	tests := []struct {
		name    string
		in      string
		wantErr bool
	}{
		{"valid", "MGIT-1.2.3", false},
		{"valid dashed", "MTIX-30-probe", false},
		{"empty", "", true},
		{"grammar violation", "not a task!!", true},
		{"path separator", "MGIT-1/2", true},
		{"control char", "MGIT-1\x00", true},
		{"oversized", "MGIT-" + strings.Repeat("1", maxArgLen), true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.wantErr, validateTaskID(tt.in) != nil)
		})
	}
}

func TestValidatePath(t *testing.T) {
	tests := []struct {
		name    string
		in      string
		wantErr bool
	}{
		{"valid", "/tmp/wt", false},
		{"valid relative", "wt/sub", false},
		{"empty", "", true},
		{"oversized", strings.Repeat("a", maxArgLen+1), true},
		{"nul", "a\x00b", true},
		{"control char", "a\x01b", true},
		{"traversal", "a/../b", true},
		{"backslash traversal", "a\\..\\b", true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.wantErr, validatePath(tt.in) != nil)
		})
	}
}

func TestValidateText(t *testing.T) {
	assert.NoError(t, validateText("m", "a normal\nmessage\twith tabs"))
	assert.Error(t, validateText("m", strings.Repeat("a", maxArgLen+1)))
	assert.Error(t, validateText("m", "a\x00b"))
}

func TestValidateToken(t *testing.T) {
	assert.NoError(t, validateToken("t", "abc123def"))
	assert.Error(t, validateToken("t", strings.Repeat("a", maxArgLen+1)))
	assert.Error(t, validateToken("t", "has space"))
	assert.Error(t, validateToken("t", "ctrl\x01"))
}

func TestIsControlChar(t *testing.T) {
	assert.False(t, isControlChar('\t'))
	assert.False(t, isControlChar('\n'))
	assert.False(t, isControlChar('\r'))
	assert.False(t, isControlChar('a'))
	assert.True(t, isControlChar('\x00'))
	assert.True(t, isControlChar('\x1f'))
	assert.True(t, isControlChar('\x7f'))
}
