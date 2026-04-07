package model

import (
	"encoding/json"
	"fmt"
	"regexp"
	"strings"
)

// taskIDPattern validates task IDs in the format PREFIX-N[.N[.N]].
// Supports 1 to 3 numeric segments after an uppercase alphabetic prefix.
// Examples: MGIT-1, MGIT-1.2, MGIT-1.2.3, PROJ-4.2.1
// Refs: FR-4, MGIT-2.1.3
var taskIDPattern = regexp.MustCompile(`^([A-Z]+)-(\d+(?:\.\d+){0,2})$`)

// TaskID represents a dot-notation task identifier.
// It is a value type that can be used as a map key.
// The format is PREFIX-N[.N[.N]] (e.g., MGIT-1.2.3).
// Refs: FR-4, MGIT-2.1.3
type TaskID struct {
	prefix string
	parts  string // dot-separated numeric parts, e.g., "1.2.3"
}

// ParseTaskID parses a dot-notation task ID string.
// Returns ErrInvalidTaskID if the format does not match.
// Refs: FR-4
func ParseTaskID(s string) (TaskID, error) {
	matches := taskIDPattern.FindStringSubmatch(s)
	if matches == nil {
		return TaskID{}, fmt.Errorf("%w: %q", ErrInvalidTaskID, s)
	}
	return TaskID{
		prefix: matches[1],
		parts:  matches[2],
	}, nil
}

// Validate checks that the TaskID is well-formed and non-zero.
// Refs: FR-4
func (t TaskID) Validate() error {
	if t.prefix == "" || t.parts == "" {
		return fmt.Errorf("%w: empty task ID", ErrInvalidTaskID)
	}
	return nil
}

// String returns the canonical string representation (e.g., "MGIT-1.2.3").
func (t TaskID) String() string {
	if t.prefix == "" {
		return ""
	}
	return t.prefix + "-" + t.parts
}

// Prefix returns the project prefix (e.g., "MGIT").
func (t TaskID) Prefix() string {
	return t.prefix
}

// Depth returns the number of numeric segments (1, 2, or 3).
func (t TaskID) Depth() int {
	if t.parts == "" {
		return 0
	}
	return strings.Count(t.parts, ".") + 1
}

// Parent returns the parent TaskID by removing the last segment.
// Returns false if the TaskID has only one segment (no parent).
// Refs: FR-4 (subtree queries)
func (t TaskID) Parent() (TaskID, bool) {
	idx := strings.LastIndex(t.parts, ".")
	if idx < 0 {
		return TaskID{}, false
	}
	return TaskID{
		prefix: t.prefix,
		parts:  t.parts[:idx],
	}, true
}

// IsZero returns true if the TaskID is the zero value.
func (t TaskID) IsZero() bool {
	return t.prefix == "" && t.parts == ""
}

// MarshalJSON implements json.Marshaler for TaskID.
func (t TaskID) MarshalJSON() ([]byte, error) {
	return json.Marshal(t.String())
}

// UnmarshalJSON implements json.Unmarshaler for TaskID.
func (t *TaskID) UnmarshalJSON(data []byte) error {
	var s string
	if err := json.Unmarshal(data, &s); err != nil {
		return fmt.Errorf("unmarshal task ID: %w", err)
	}
	parsed, err := ParseTaskID(s)
	if err != nil {
		return err
	}
	*t = parsed
	return nil
}
