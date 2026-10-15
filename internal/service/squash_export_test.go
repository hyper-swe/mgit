package service

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/hyper-swe/mgit/internal/model"
	gitstore "github.com/hyper-swe/mgit/internal/store/git"
)

// --- mbox header tests (the preamble GitFormatPatch writes) ---
// Refs: FR-7, MGIT-4.2.2, MGIT-114

func TestSquashService_MboxHeader_BasicCommit(t *testing.T) {
	svc := (&SquashService{}).WithPatchAuthor(testPatchAuthor)
	c := &model.Commit{
		CommitID:  "abc123def456",
		AgentID:   "agent-01",
		Message:   "implement feature X",
		CreatedAt: time.Date(2026, 4, 7, 12, 0, 0, 0, time.UTC),
	}
	out := svc.mboxHeader(c, testPatchIdentity)

	assert.Contains(t, out, "From abc123def456")
	assert.Contains(t, out, "From: Test Author <author@example.invalid>")
	assert.NotContains(t, out, "agent-01", "the commit's agent is not the patch's author")
	assert.Contains(t, out, "Subject: [PATCH] [squashed] implement feature X")
	assert.Contains(t, out, "---")
}

func TestSquashService_MboxHeader_AlreadySquashedPrefix(t *testing.T) {
	svc := (&SquashService{}).WithPatchAuthor(testPatchAuthor)
	c := &model.Commit{
		CommitID:  "abc123",
		AgentID:   "agent-01",
		Message:   "[squashed] already prefixed",
		CreatedAt: time.Date(2026, 4, 7, 12, 0, 0, 0, time.UTC),
	}
	out := svc.mboxHeader(c, testPatchIdentity)

	// Should not double-prefix.
	assert.Contains(t, out, "Subject: [PATCH] [squashed] already prefixed")
	assert.NotContains(t, out, "[squashed] [squashed]")
}

func TestSquashService_MboxHeader_WithBody(t *testing.T) {
	svc := (&SquashService{}).WithPatchAuthor(testPatchAuthor)
	c := &model.Commit{
		CommitID:  "abc123",
		AgentID:   "agent-01",
		Message:   "subject line\n\nDetailed body paragraph.",
		CreatedAt: time.Date(2026, 4, 7, 12, 0, 0, 0, time.UTC),
	}
	out := svc.mboxHeader(c, testPatchIdentity)

	assert.Contains(t, out, "Subject: [PATCH] [squashed] subject line")
	assert.Contains(t, out, "Detailed body paragraph.")
}

func TestSquashService_MboxHeader_EmptyAgentID(t *testing.T) {
	svc := (&SquashService{}).WithPatchAuthor(testPatchAuthor)
	c := &model.Commit{
		CommitID:  "abc123",
		Message:   "test",
		CreatedAt: time.Date(2026, 4, 7, 12, 0, 0, 0, time.UTC),
	}
	out := svc.mboxHeader(c, testPatchIdentity)

	// The exporter authors the patch whatever the commit's agent: mgit's
	// internal identity never reaches it. Refs: MGIT-237
	assert.Contains(t, out, "From: Test Author <author@example.invalid>")
	assert.NotContains(t, out, "mgit-squash")
	assert.NotContains(t, out, "mgit.local")
}

func TestSquashService_MboxHeader_ZeroCreatedAt(t *testing.T) {
	svc := (&SquashService{}).WithPatchAuthor(testPatchAuthor)
	c := &model.Commit{
		CommitID: "abc123",
		AgentID:  "agent-01",
		Message:  "test",
		// CreatedAt is zero value.
	}
	out := svc.mboxHeader(c, testPatchIdentity)

	// Should still produce valid output (uses time.Now() as fallback).
	assert.Contains(t, out, "From:")
	assert.Contains(t, out, "Date:")
}

func TestSquashService_MboxHeader_EmptyCommitID(t *testing.T) {
	svc := (&SquashService{}).WithPatchAuthor(testPatchAuthor)
	c := &model.Commit{
		ContentHash: "sha256hash",
		AgentID:     "agent-01",
		Message:     "test",
		CreatedAt:   time.Date(2026, 4, 7, 12, 0, 0, 0, time.UTC),
	}
	out := svc.mboxHeader(c, testPatchIdentity)

	// When CommitID is empty, falls back to ContentHash.
	assert.Contains(t, out, "From sha256hash")
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

// testPatchIdentity is the exporter the service tests inject, so no test
// reads the machine's git config. Refs: MGIT-237
var testPatchIdentity = gitstore.AuthorIdentity{Name: "Test Author", Email: "author@example.invalid"}

func testPatchAuthor() (gitstore.AuthorIdentity, error) { return testPatchIdentity, nil }

// With no identity wired, exporting a patch is refused with
// model.ErrNoPatchIdentity, and no patch text is produced. Refs: MGIT-237
func TestSquashService_NoPatchAuthor_RefusesToExport(t *testing.T) {
	svc := &SquashService{}
	_, err := svc.PatchAuthor()
	require.ErrorIs(t, err, model.ErrNoPatchIdentity)
	out, err := svc.GitFormatPatch(context.Background(), &model.Commit{CommitID: "abc", Message: "m"})
	require.ErrorIs(t, err, model.ErrNoPatchIdentity)
	assert.Empty(t, out, "no identity, no patch")
}
