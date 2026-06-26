package model

import (
	"encoding/json"
	"fmt"
	"regexp"
	"strings"
)

// taskIDGrammar is the human-readable description of the accepted task-id
// form. It is embedded in rejection errors so a user or agent can self-correct.
// Refs: FR-4, MGIT-41
const taskIDGrammar = "<PREFIX>-<segments> using letters/digits/.-_ " +
	"(e.g. MTIX-30.6, MGIT-1.2.3, MTIX-30-probe)"

// taskIDPattern validates task IDs in the broadened form PREFIX-BODY.
//
// PREFIX is an alphanumeric project token (e.g. MGIT, MTIX, PROJ2). BODY is a
// path of segments separated by "." or "-", where each segment is one or more
// of [A-Za-z0-9_]. This accepts the real-world ids mtix and users emit —
// MTIX-30-probe, MTIX-30.6, MGIT-1.2.3, MGIT-11.13.5 — while staying a clean,
// git-ref-safe and log/SQL-safe token.
//
// The character class is an ALLOWLIST (the safe set) rather than a blocklist:
// path separators (/ \), whitespace, control chars, and shell/glob/SQL
// metacharacters (space, quotes, ; | & $ backtick * ? ~ etc.) are all absent
// from the class and therefore rejected. Empty segments are impossible (each
// segment needs at least one char), so leading/trailing/doubled separators and
// ".." sequences cannot match. ids derive git branch names (task/<id>),
// commit-message tags ([MGIT:<id>]), and SQLite params, so this safety
// boundary is load-bearing.
//
// Examples accepted: MGIT-1, MGIT-1.2.3, PROJ-4.2.1, MTIX-30-probe, MGIT-1_a.
// Refs: FR-4, MGIT-2.1.3, MGIT-41
var taskIDPattern = regexp.MustCompile(
	`^([A-Za-z0-9]+)-([A-Za-z0-9_]+(?:[.-][A-Za-z0-9_]+)*)$`)

// TaskID represents a prefixed, segmented task identifier.
// It is a value type that can be used as a map key.
// The form is PREFIX-BODY where BODY is a "."/"-"-separated segment path
// (e.g., MGIT-1.2.3, MTIX-30-probe).
// Refs: FR-4, MGIT-2.1.3, MGIT-41
type TaskID struct {
	prefix string
	parts  string // separated segment path, e.g., "1.2.3" or "30-probe"
}

// ParseTaskID parses a task ID string into the broadened PREFIX-BODY form.
// Returns ErrInvalidTaskID, naming the accepted grammar and quoting the
// offending value, when the input does not match.
// Refs: FR-4, MGIT-41
func ParseTaskID(s string) (TaskID, error) {
	matches := taskIDPattern.FindStringSubmatch(s)
	if matches == nil {
		return TaskID{}, fmt.Errorf("%w %q: must match %s", ErrInvalidTaskID, s, taskIDGrammar)
	}
	return TaskID{
		prefix: matches[1],
		parts:  matches[2],
	}, nil
}

// validateTaskIDField validates a required task_id field value for use
// inside Validate() methods: non-empty and well-formed per ParseTaskID.
// On malformed (non-empty) input it surfaces the accepted grammar so the
// caller learns the fix, not just that it failed.
// Shared by worktree and sandbox option types. Refs: FR-4, FR-16, FR-17.1, MGIT-41
func validateTaskIDField(id string) error {
	if id == "" {
		return &ValidationError{Field: "task_id", Message: "must not be empty"}
	}
	if _, err := ParseTaskID(id); err != nil {
		return &ValidationError{
			Field:   "task_id",
			Message: fmt.Sprintf("invalid task id %q: must match %s", id, taskIDGrammar),
		}
	}
	return nil
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

// Depth returns the number of dot-separated segments in the body
// (e.g. MGIT-1.2.3 -> 3, MTIX-30-probe -> 1). Dash-joined segments count
// as a single dotted segment. Refs: MGIT-41
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
