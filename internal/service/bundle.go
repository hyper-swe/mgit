package service

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/hyper-swe/mgit/internal/model"
	"github.com/hyper-swe/mgit/internal/store/index"
)

// BundleVersion is the on-disk format version emitted by Export and
// expected by Import.
const BundleVersion = "1"

// BundleFormat identifies a file as an mgit bundle.
const BundleFormat = "mgit-bundle"

// ImportMode controls how an Import call merges a bundle into an
// existing repository.
type ImportMode string

const (
	// ImportMerge adds bundle records that do not already exist in the
	// destination index. Duplicate (task_id, commit_hash) records are
	// skipped silently.
	ImportMerge ImportMode = "merge"
	// ImportReplace requires the destination index to be empty for the
	// imported task IDs. Any conflict aborts the import — task_commits
	// is append-only and cannot be deleted.
	ImportReplace ImportMode = "replace"
)

// Bundle is the on-wire representation of an mgit bundle archive.
// Refs: FR-12.4, FR-12.5, MGIT-4.2.12
type Bundle struct {
	Version     string               `json:"version"`
	Format      string               `json:"format"`
	CreatedAt   string               `json:"created_at"`
	Manifest    BundleManifest       `json:"manifest"`
	TaskCommits []index.CommitRecord `json:"task_commits"`
}

// BundleManifest carries the integrity metadata for a Bundle.
// Refs: FR-12.4
type BundleManifest struct {
	ChecksumSHA256 string `json:"checksum_sha256"`
	CommitCount    int    `json:"commit_count"`
}

// ImportResult reports the outcome of an Import call, suitable for JSON
// output to CLI consumers.
// Refs: MGIT-4.2.12
type ImportResult struct {
	Mode     ImportMode `json:"mode"`
	Imported int        `json:"imported"`
	Skipped  int        `json:"skipped"`
	Total    int        `json:"total"`
	Status   string     `json:"status"`
}

// BundleService exports and imports mgit bundle archives, verifying
// SHA-256 manifest checksums on every read.
// Refs: FR-12.4, FR-12.5, MGIT-4.2.12
type BundleService struct {
	indexStore *index.Store
	clock      func() time.Time
}

// NewBundleService creates a BundleService with the given dependencies.
func NewBundleService(idx *index.Store, clock func() time.Time) *BundleService {
	if clock == nil {
		clock = func() time.Time { return time.Now().UTC() }
	}
	return &BundleService{indexStore: idx, clock: clock}
}

// Export collects the task_commits records for the given task IDs and
// produces a JSON bundle with a SHA-256 manifest checksum. Pass an empty
// slice to export every record currently visible to GetTaskCommits via
// the supplied taskIDs argument; the caller is responsible for assembling
// the desired task list.
// Refs: FR-12.4
func (s *BundleService) Export(ctx context.Context, taskIDs []string) ([]byte, error) {
	var records []index.CommitRecord
	for _, taskID := range taskIDs {
		recs, err := s.indexStore.GetTaskCommits(ctx, taskID)
		if err != nil {
			return nil, fmt.Errorf("bundle export: get %s: %w", taskID, err)
		}
		records = append(records, recs...)
	}

	checksum, err := canonicalChecksum(records)
	if err != nil {
		return nil, fmt.Errorf("bundle export: checksum: %w", err)
	}

	bundle := Bundle{
		Version:   BundleVersion,
		Format:    BundleFormat,
		CreatedAt: s.clock().UTC().Format(time.RFC3339),
		Manifest: BundleManifest{
			ChecksumSHA256: checksum,
			CommitCount:    len(records),
		},
		TaskCommits: records,
	}
	data, err := json.MarshalIndent(bundle, "", "  ")
	if err != nil {
		return nil, fmt.Errorf("bundle export: marshal: %w", err)
	}
	return data, nil
}

// Import parses a bundle, verifies its manifest checksum, and writes the
// records into the index according to the chosen mode.
// Refs: FR-12.5
func (s *BundleService) Import(ctx context.Context, data []byte, mode ImportMode) (*ImportResult, error) {
	if mode == "" {
		mode = ImportMerge
	}
	if mode != ImportMerge && mode != ImportReplace {
		return nil, fmt.Errorf("bundle import: unknown mode %q", mode)
	}

	var bundle Bundle
	if err := json.Unmarshal(data, &bundle); err != nil {
		return nil, fmt.Errorf("bundle import: %w: parse: %w", model.ErrVerificationFailed, err)
	}
	if bundle.Format != BundleFormat {
		return nil, fmt.Errorf("bundle import: %w: not an mgit bundle (format=%q)",
			model.ErrVerificationFailed, bundle.Format)
	}
	if bundle.Version != BundleVersion {
		return nil, fmt.Errorf("bundle import: %w: unsupported version %q",
			model.ErrVerificationFailed, bundle.Version)
	}

	// Recompute the checksum and compare with the manifest.
	computed, err := canonicalChecksum(bundle.TaskCommits)
	if err != nil {
		return nil, fmt.Errorf("bundle import: checksum: %w", err)
	}
	if computed != bundle.Manifest.ChecksumSHA256 {
		return nil, fmt.Errorf("bundle import: %w: manifest checksum mismatch (got %s, want %s)",
			model.ErrVerificationFailed, computed, bundle.Manifest.ChecksumSHA256)
	}
	if bundle.Manifest.CommitCount != len(bundle.TaskCommits) {
		return nil, fmt.Errorf("bundle import: %w: commit count mismatch", model.ErrVerificationFailed)
	}

	// For replace mode: refuse if the destination already holds any of
	// the bundle's task IDs (task_commits is append-only — we cannot
	// delete and re-insert).
	if mode == ImportReplace {
		seen := map[string]bool{}
		for _, r := range bundle.TaskCommits {
			if seen[r.TaskID] {
				continue
			}
			seen[r.TaskID] = true
			existing, err := s.indexStore.GetTaskCommits(ctx, r.TaskID)
			if err != nil {
				return nil, fmt.Errorf("bundle import (replace): probe %s: %w", r.TaskID, err)
			}
			if len(existing) > 0 {
				return nil, fmt.Errorf(
					"bundle import (replace): task %s already has %d commits — replace requires an empty target (task_commits is append-only)",
					r.TaskID, len(existing))
			}
		}
	}

	// Walk the records and insert them. In merge mode, duplicates are
	// skipped silently.
	imported, skipped := 0, 0
	for _, r := range bundle.TaskCommits {
		err := s.indexStore.AddCommitToTask(
			ctx, r.TaskID, r.CommitHash, r.ContentHash, r.AgentID, r.Position,
		)
		if err != nil {
			if mode == ImportMerge && isDuplicateInsert(err) {
				skipped++
				continue
			}
			return nil, fmt.Errorf("bundle import: insert %s/%s: %w",
				r.TaskID, r.CommitHash, err)
		}
		imported++
	}

	return &ImportResult{
		Mode:     mode,
		Imported: imported,
		Skipped:  skipped,
		Total:    len(bundle.TaskCommits),
		Status:   "imported",
	}, nil
}

// canonicalChecksum returns the SHA-256 over the canonical JSON
// serialization of the given records. Two semantically equivalent record
// slices produce the same checksum.
func canonicalChecksum(records []index.CommitRecord) (string, error) {
	// json.Marshal of a slice is deterministic for record fields with
	// concrete (non-map) types, which is the case for CommitRecord.
	data, err := json.Marshal(records)
	if err != nil {
		return "", fmt.Errorf("marshal: %w", err)
	}
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:]), nil
}

// isDuplicateInsert returns true when the SQLite driver reports a unique
// constraint violation against task_commits.
func isDuplicateInsert(err error) bool {
	if err == nil {
		return false
	}
	msg := err.Error()
	return strings.Contains(msg, "UNIQUE constraint failed") ||
		strings.Contains(msg, "constraint failed: UNIQUE")
}
