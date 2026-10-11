package main

import (
	"testing"

	"github.com/stretchr/testify/require"
)

// Refs: MGIT-296, FR-2.9. Inspect stored bytes, not rendered commit output.
func TestCommit_RepeatedMessages(t *testing.T) {
	for _, tt := range []struct {
		name string
		args []string
		want string
	}{
		{"short", []string{"-m", "a", "-m", "b"}, "a\n\nb"},
		{"mixed", []string{"--message", "a", "-m", "b", "--message=c"}, "a\n\nb\n\nc"},
		{"single", []string{"-m", "a,b\nbody\n"}, "a,b\nbody\n"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			root := seedRepoForMessageFile(t)
			args := append([]string{"commit", "--allow-empty", "--task-id", "MGIT-296"}, tt.args...)
			require.NoError(t, runCLI(t, args...))
			require.Equal(t, "[MGIT:MGIT-296] "+tt.want, commitMessageBytes(t, root, headCommitID(t, root)))
		})
	}
}
