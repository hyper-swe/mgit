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

// FileDiffMode identifies the git file mode an added or modified diff entry
// should be recorded with. Its zero value is FileModeRegular, so a FileDiff
// constructed without setting Mode behaves exactly as a regular 100644 file —
// preserving backward compatibility with every existing construction site.
// Refs: FR-11, MGIT-16
type FileDiffMode int

// Supported file modes for a diff-applied entry. The zero value
// (FileModeRegular) maps to git mode 100644; FileModeExecutable maps to 100755
// and FileModeSymlink to 120000. Refs: FR-11, MGIT-16
const (
	// FileModeRegular is a regular file (git mode 100644). It is the zero
	// value, so an unset FileDiff.Mode means a regular file.
	FileModeRegular FileDiffMode = iota
	// FileModeExecutable is an executable file (git mode 100755).
	FileModeExecutable
	// FileModeSymlink is a symbolic link (git mode 120000); its blob content
	// is the link target text.
	FileModeSymlink
)

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
	// Mode is the git file mode for an added/modified entry. Its zero value
	// (FileModeRegular) means a regular 100644 file, so leaving it unset keeps
	// pre-existing behavior; set it to thread an executable (100755) or symlink
	// (120000) entry through the diff-apply path. Ignored for deletions.
	// Refs: FR-11, MGIT-16
	Mode FileDiffMode `json:"mode,omitempty"`
	// OldMode is the git file mode of the PRE-change entry (the From side of a
	// modify/delete). It lets an inverse application (rollback) restore the
	// original object type — without it a regular↔symlink type change would be
	// reverted with the post-change mode, corrupting the restored entry.
	// Zero value means regular, mirroring Mode. Refs: MGIT-54
	OldMode FileDiffMode `json:"old_mode,omitempty"`
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
