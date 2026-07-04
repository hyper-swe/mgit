package service

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/hyper-swe/mgit/internal/model"
	gitstore "github.com/hyper-swe/mgit/internal/store/git"
)

// RestoreService restores file content from a specific commit without
// performing a rollback or creating a new commit — per file (RestoreFile) or
// the whole working tree (RestoreAll, MGIT-55).
// Refs: FR-6.7, MGIT-4.2.8, MGIT-55
type RestoreService struct {
	commitStore *gitstore.CommitStore
	repo        *gitstore.Repository
	repoRoot    string
}

// NewRestoreService creates a RestoreService backed by the given repository
// and commit store. repoRoot is the working-directory root into which
// restored files will be written.
func NewRestoreService(repo *gitstore.Repository, cs *gitstore.CommitStore, repoRoot string) *RestoreService {
	return &RestoreService{
		commitStore: cs,
		repo:        repo,
		repoRoot:    repoRoot,
	}
}

// RestoreAllResult holds the outcome of a whole-tree restore.
// Refs: MGIT-55
type RestoreAllResult struct {
	CommitHash   string `json:"commit_hash"`
	FilesChanged int    `json:"files_changed"`
	Status       string `json:"status"`
}

// RestoreAll returns the entire working tree to the state of the given
// checkpoint commit, comparing the checkpoint against the DISK state (not
// the committed tree), so it recovers a trashed-but-uncommitted tree — the
// scenario checkpoint recovery exists for. Files are rewritten to the
// checkpoint's content/mode; paths tracked at the tip but absent from the
// checkpoint are removed; untracked files unknown to both are left alone.
//
// Safety: when the restore would overwrite UNCOMMITTED local state (staged
// paths, or disk diverging from the current tip) it refuses with
// ErrContentConflict unless force is set — recovery of a trashed tree is
// exactly that overwrite, so it is an explicit `--force` away, never silent.
//
// No commit is minted; the restored paths are STAGED so the agent's next
// commit lands them as its task step — and so the status-time auto-resync
// treats them as task work-in-progress rather than absorbing them into an
// anonymous base commit (the MGIT-56 failure mode). Restoring to a state
// disk already matches is an idempotent no-op.
// Refs: MGIT-55, FR-6.7 (review findings M1, M2)
func (s *RestoreService) RestoreAll(ctx context.Context, hash string, force bool) (*RestoreAllResult, error) {
	target, err := s.commitStore.GetCommit(ctx, hash)
	if err != nil {
		return nil, fmt.Errorf("restore --all: %w", err)
	}

	apply, uncommitted, err := s.repo.ComputeRestoreDiffs(target.CommitID)
	if err != nil {
		return nil, fmt.Errorf("restore --all: %w", err)
	}
	if len(apply) == 0 {
		return &RestoreAllResult{CommitHash: target.CommitID, Status: "unchanged"}, nil
	}
	if len(uncommitted) > 0 && !force {
		return nil, fmt.Errorf("restore --all: %w: would overwrite uncommitted local changes on %s "+
			"(pass --force to restore over them)", model.ErrContentConflict, strings.Join(uncommitted, ", "))
	}

	if err := s.repo.MaterializeDiffs(apply); err != nil {
		return nil, fmt.Errorf("restore --all: %w", err)
	}
	// Stage the restored paths (task WIP for the next commit; keeps the
	// auto-resync from absorbing them — MGIT-56).
	if err := s.stageRestored(apply); err != nil {
		return nil, fmt.Errorf("restore --all: %w", err)
	}
	return &RestoreAllResult{
		CommitHash:   target.CommitID,
		FilesChanged: len(apply),
		Status:       "restored",
	}, nil
}

// stageRestored adds the restored paths to the staging selection, preserving
// whatever was already staged. Refs: MGIT-55 (review finding M2)
func (s *RestoreService) stageRestored(apply []model.FileDiff) error {
	staged, err := s.repo.StagedSnapshot()
	if err != nil {
		return fmt.Errorf("snapshot staging: %w", err)
	}
	have := make(map[string]bool, len(staged))
	for _, p := range staged {
		have[p] = true
	}
	for _, d := range apply {
		if !have[d.Path] {
			staged = append(staged, d.Path)
			have[d.Path] = true
		}
	}
	if err := s.repo.RestoreStaging(staged); err != nil {
		return fmt.Errorf("stage restored paths: %w", err)
	}
	return nil
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
