package service

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

// Only the tag mgit wrote is removed: the leading one, and the one a
// cherry-pick copies from its source. The same characters written by an
// author elsewhere in a message stay. Refs: MGIT-228
func TestWithoutTaskTag_RemovesOnlyTheTagMgitWrote(t *testing.T) {
	tests := []struct{ name, in, want string }{
		{"a task commit", "[MGIT:wi-2] docs(cli): tidy", "docs(cli): tidy"},
		{"no tag", "docs(cli): tidy", "docs(cli): tidy"},
		{"a tag with no space after it", "[MGIT:wi-2]docs", "docs"},
		{"a revert", "[MGIT:wi-2] Revert: wrong turn (2 commits)", "Revert: wrong turn (2 commits)"},
		{"a cherry-pick of a tagged commit", "[MGIT:wi-3] cherry-pick 1a2b3c4d: [MGIT:wi-2] keep this", "cherry-pick 1a2b3c4d: keep this"},
		{"a cherry-pick of a cherry-pick", "[MGIT:wi-4] cherry-pick 5e6f7a8b: [MGIT:wi-3] cherry-pick 1a2b3c4d: [MGIT:wi-2] keep this",
			"cherry-pick 5e6f7a8b: cherry-pick 1a2b3c4d: keep this"},
		{"dotted task ids", "[MGIT:MGIT-5.1.2] cherry-pick 1a2b3c4d: [MGIT:MGIT-5.1] step", "cherry-pick 1a2b3c4d: step"},
		{"an author's own mention", "docs: explain the [MGIT:<task>] tag", "docs: explain the [MGIT:<task>] tag"},
		{"multi-line body kept", "[MGIT:wi-2] subject\n\nbody line", "subject\n\nbody line"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, withoutTaskTag(tt.in))
		})
	}
}
