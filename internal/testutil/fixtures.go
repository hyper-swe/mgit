package testutil

import (
	"crypto/sha256"
	"fmt"
	"time"
)

// TestCommit represents a minimal commit for testing.
// This mirrors the fields that model.Commit will have in Wave 2.
// Refs: FR-3, MGIT-1.2.5
type TestCommit struct {
	ID        string
	TaskID    string
	Hash      string
	Message   string
	Author    string
	CreatedAt time.Time
}

// TestBranch represents a minimal branch for testing.
// Refs: FR-5, MGIT-1.2.5
type TestBranch struct {
	Name   string
	TaskID string
	Head   string
}

// TestDiff represents a minimal file diff for testing.
// Refs: FR-11, MGIT-1.2.5
type TestDiff struct {
	Path      string
	Operation string
	OldHash   string
	NewHash   string
}

// MakeTestCommit creates a TestCommit with sensible defaults.
func MakeTestCommit() TestCommit {
	now := time.Date(2026, 4, 7, 12, 0, 0, 0, time.UTC)
	hash := fmt.Sprintf("%x", sha256.Sum256([]byte("test-commit")))
	return TestCommit{
		ID:        hash[:8],
		TaskID:    "TEST-1.1.1",
		Hash:      hash,
		Message:   "[MGIT:TEST-1.1.1] test commit",
		Author:    "test-agent",
		CreatedAt: now,
	}
}

// MakeTestBranch creates a TestBranch with sensible defaults.
func MakeTestBranch() TestBranch {
	return TestBranch{
		Name:   "task/TEST-1.1",
		TaskID: "TEST-1.1",
		Head:   fmt.Sprintf("%x", sha256.Sum256([]byte("test-branch")))[:40],
	}
}

// MakeTestDiff creates a TestDiff with sensible defaults.
func MakeTestDiff() TestDiff {
	return TestDiff{
		Path:      "internal/model/commit.go",
		Operation: "added",
		OldHash:   "",
		NewHash:   fmt.Sprintf("%x", sha256.Sum256([]byte("new-content")))[:40],
	}
}
