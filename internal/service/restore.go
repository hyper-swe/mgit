package service

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	gitstore "github.com/hyper-swe/mgit/internal/store/git"
)

// RestoreService restores a single file from a specific commit without
// performing a rollback or creating a new commit.
// Refs: FR-6.7, MGIT-4.2.8
type RestoreService struct {
	commitStore *gitstore.CommitStore
	repoRoot    string
}

// NewRestoreService creates a RestoreService backed by the given commit
// store. repoRoot is the working-directory root into which restored
// files will be written.
func NewRestoreService(cs *gitstore.CommitStore, repoRoot string) *RestoreService {
	return &RestoreService{
		commitStore: cs,
		repoRoot:    repoRoot,
	}
}

// RestoreResult holds the outcome of a restore operation, suitable for
// JSON serialization to CLI consumers.
// Refs: FR-6.7, MGIT-4.2.8
type RestoreResult struct {
	Path       string `json:"path"`
	CommitHash string `json:"commit_hash"`
	BytesWrit  int    `json:"bytes_written"`
	Status     string `json:"status"`
}

// RestoreFile reads `path` from the tree of commit `hash` and writes it
// to the working directory at `repoRoot/path`. The function refuses to
// escape the working directory via `..` segments.
// Refs: FR-6.7
func (s *RestoreService) RestoreFile(ctx context.Context, path, hash string) (*RestoreResult, error) {
	if path == "" {
		return nil, fmt.Errorf("restore: path must not be empty")
	}
	if hash == "" {
		return nil, fmt.Errorf("restore: commit hash must not be empty")
	}

	// Reject path traversal.
	cleaned := filepath.Clean(path)
	if strings.HasPrefix(cleaned, "..") || filepath.IsAbs(cleaned) {
		return nil, fmt.Errorf("restore: refusing path %q (must be relative, no traversal)", path)
	}

	contents, err := s.commitStore.GetFileFromCommit(ctx, hash, cleaned)
	if err != nil {
		return nil, fmt.Errorf("restore: %w", err)
	}

	target := filepath.Join(s.repoRoot, cleaned)
	// Ensure parent directory exists.
	if err := os.MkdirAll(filepath.Dir(target), 0o750); err != nil {
		return nil, fmt.Errorf("restore: create parent dir: %w", err)
	}
	if err := os.WriteFile(target, contents, 0o600); err != nil {
		return nil, fmt.Errorf("restore: write file: %w", err)
	}

	return &RestoreResult{
		Path:       cleaned,
		CommitHash: hash,
		BytesWrit:  len(contents),
		Status:     "restored",
	}, nil
}
