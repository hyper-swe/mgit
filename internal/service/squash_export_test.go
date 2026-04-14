package service

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"

	"github.com/hyper-swe/mgit-dev/internal/model"
)

// --- ExportToGitPatch Tests ---
// Refs: FR-7, MGIT-4.2.2

func TestSquashService_ExportToGitPatch_NilCommit(t *testing.T) {
	svc := &SquashService{}
	out := svc.ExportToGitPatch(nil)
	assert.Empty(t, out)
}

func TestSquashService_ExportToGitPatch_BasicCommit(t *testing.T) {
	svc := &SquashService{}
	c := &model.Commit{
		CommitID:  "abc123def456",
		AgentID:   "agent-01",
		Message:   "implement feature X",
		CreatedAt: time.Date(2026, 4, 7, 12, 0, 0, 0, time.UTC),
	}
	out := svc.ExportToGitPatch(c)

	assert.Contains(t, out, "From abc123def456")
	assert.Contains(t, out, "From: agent-01 <agent-01@mgit.local>")
	assert.Contains(t, out, "Subject: [PATCH] [squashed] implement feature X")
	assert.Contains(t, out, "---")
	assert.Contains(t, out, "-- \nmgit\n")
}

func TestSquashService_ExportToGitPatch_AlreadySquashedPrefix(t *testing.T) {
	svc := &SquashService{}
	c := &model.Commit{
		CommitID:  "abc123",
		AgentID:   "agent-01",
		Message:   "[squashed] already prefixed",
		CreatedAt: time.Date(2026, 4, 7, 12, 0, 0, 0, time.UTC),
	}
	out := svc.ExportToGitPatch(c)

	// Should not double-prefix.
	assert.Contains(t, out, "Subject: [PATCH] [squashed] already prefixed")
	assert.NotContains(t, out, "[squashed] [squashed]")
}

func TestSquashService_ExportToGitPatch_WithBody(t *testing.T) {
	svc := &SquashService{}
	c := &model.Commit{
		CommitID:  "abc123",
		AgentID:   "agent-01",
		Message:   "subject line\n\nDetailed body paragraph.",
		CreatedAt: time.Date(2026, 4, 7, 12, 0, 0, 0, time.UTC),
	}
	out := svc.ExportToGitPatch(c)

	assert.Contains(t, out, "Subject: [PATCH] [squashed] subject line")
	assert.Contains(t, out, "Detailed body paragraph.")
}

func TestSquashService_ExportToGitPatch_EmptyAgentID(t *testing.T) {
	svc := &SquashService{}
	c := &model.Commit{
		CommitID:  "abc123",
		Message:   "test",
		CreatedAt: time.Date(2026, 4, 7, 12, 0, 0, 0, time.UTC),
	}
	out := svc.ExportToGitPatch(c)

	// Should default to mgit-squash.
	assert.Contains(t, out, "From: mgit-squash <mgit-squash@mgit.local>")
}

func TestSquashService_ExportToGitPatch_ZeroCreatedAt(t *testing.T) {
	svc := &SquashService{}
	c := &model.Commit{
		CommitID: "abc123",
		AgentID:  "agent-01",
		Message:  "test",
		// CreatedAt is zero value.
	}
	out := svc.ExportToGitPatch(c)

	// Should still produce valid output (uses time.Now() as fallback).
	assert.Contains(t, out, "From:")
	assert.Contains(t, out, "Date:")
}

func TestSquashService_ExportToGitPatch_EmptyCommitID(t *testing.T) {
	svc := &SquashService{}
	c := &model.Commit{
		ContentHash: "sha256hash",
		AgentID:     "agent-01",
		Message:     "test",
		CreatedAt:   time.Date(2026, 4, 7, 12, 0, 0, 0, time.UTC),
	}
	out := svc.ExportToGitPatch(c)

	// When CommitID is empty, falls back to ContentHash.
	assert.Contains(t, out, "From sha256hash")
}

func TestSquashService_ExportToGitPatch_WithFileDiffs(t *testing.T) {
	svc := &SquashService{}
	c := &model.Commit{
		CommitID:  "abc123",
		AgentID:   "agent-01",
		Message:   "add and modify files",
		CreatedAt: time.Date(2026, 4, 7, 12, 0, 0, 0, time.UTC),
		FileDiffs: []model.FileDiff{
			{
				Path: "new.go", Operation: model.DiffAdded,
				NewHash: "deadbeef12345678",
			},
			{
				Path: "old.go", Operation: model.DiffDeleted,
				OldHash: "abcdef12",
			},
			{
				Path: "mod.go", Operation: model.DiffModified,
				OldHash: "aaaa1111", NewHash: "bbbb2222",
				Hunks: []model.Hunk{
					{LineStart: 10, LinesAdded: 3, LinesRemoved: 1, Content: "+added\n-removed\n"},
				},
			},
		},
	}
	out := svc.ExportToGitPatch(c)

	assert.Contains(t, out, "diff --git a/new.go b/new.go")
	assert.Contains(t, out, "new file mode 100644")
	assert.Contains(t, out, "diff --git a/old.go b/old.go")
	assert.Contains(t, out, "deleted file mode 100644")
	assert.Contains(t, out, "diff --git a/mod.go b/mod.go")
	assert.Contains(t, out, "@@ -10,1 +10,3 @@")
	assert.Contains(t, out, "+added")
	assert.Contains(t, out, "-removed")
}

// --- splitMessage Tests ---

func TestSplitMessage(t *testing.T) {
	tests := []struct {
		name        string
		msg         string
		wantSubject string
		wantBody    string
	}{
		{
			name:        "single_line",
			msg:         "subject only",
			wantSubject: "subject only",
			wantBody:    "",
		},
		{
			name:        "subject_and_body",
			msg:         "subject line\n\nbody text",
			wantSubject: "subject line",
			wantBody:    "body text",
		},
		{
			name:        "subject_with_trailing_newline",
			msg:         "subject\n",
			wantSubject: "subject",
			wantBody:    "",
		},
		{
			name:        "empty",
			msg:         "",
			wantSubject: "",
			wantBody:    "",
		},
		{
			name:        "whitespace_subject",
			msg:         "  subject with spaces  ",
			wantSubject: "subject with spaces",
			wantBody:    "",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			subject, body := splitMessage(tt.msg)
			assert.Equal(t, tt.wantSubject, subject)
			assert.Equal(t, tt.wantBody, body)
		})
	}
}

// --- shortPatchHash Tests ---

func TestShortPatchHash(t *testing.T) {
	tests := []struct {
		name string
		hash string
		want string
	}{
		{"empty", "", "00000000"},
		{"short", "abc", "abc"},
		{"exactly_8", "12345678", "12345678"},
		{"long", "1234567890abcdef", "12345678"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, shortPatchHash(tt.hash))
		})
	}
}
