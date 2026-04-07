package model

import "fmt"

// DiffOperation represents the type of change to a file.
// Refs: FR-11, MGIT-2.1.4
type DiffOperation string

// Supported diff operations per FR-11.
const (
	DiffAdded    DiffOperation = "added"
	DiffModified DiffOperation = "modified"
	DiffDeleted  DiffOperation = "deleted"
	DiffRenamed  DiffOperation = "renamed"
)

// validOperations is the set of valid diff operations.
var validOperations = map[DiffOperation]bool{
	DiffAdded:    true,
	DiffModified: true,
	DiffDeleted:  true,
	DiffRenamed:  true,
}

// Hunk represents a contiguous block of changes within a file diff.
// Refs: FR-11, MGIT-2.1.4
type Hunk struct {
	LineStart    int    `json:"line_start"`
	LinesAdded   int    `json:"lines_added"`
	LinesRemoved int    `json:"lines_removed"`
	Content      string `json:"content"`
}

// DiffStatistics holds aggregate line change counts for a FileDiff.
// Refs: FR-11
type DiffStatistics struct {
	LinesAdded   int
	LinesRemoved int
}

// FileDiff represents a change to a single file.
// It tracks the operation type, content hashes, and detailed hunks.
// Refs: FR-11, MGIT-2.1.4
type FileDiff struct {
	Path       string        `json:"path"`
	Operation  DiffOperation `json:"operation"`
	OldHash    string        `json:"old_hash,omitempty"`
	NewHash    string        `json:"new_hash,omitempty"`
	Hunks      []Hunk        `json:"hunks,omitempty"`
	BinaryDiff bool          `json:"binary_diff,omitempty"`
}

// Validate checks that the FileDiff has a non-empty path and valid operation.
// Refs: FR-11
func (d FileDiff) Validate() error {
	if d.Path == "" {
		return fmt.Errorf("%w: path must not be empty", ErrInvalidDiff)
	}
	if !validOperations[d.Operation] {
		return fmt.Errorf("%w: invalid operation %q", ErrInvalidDiff, d.Operation)
	}
	return nil
}

// Statistics returns aggregate line change counts across all hunks.
// Binary files return zero counts.
// Refs: FR-11
func (d FileDiff) Statistics() DiffStatistics {
	var stats DiffStatistics
	if d.BinaryDiff {
		return stats
	}
	for _, h := range d.Hunks {
		stats.LinesAdded += h.LinesAdded
		stats.LinesRemoved += h.LinesRemoved
	}
	return stats
}
