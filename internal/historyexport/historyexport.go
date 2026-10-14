// Package historyexport renders a task's per-step history and audit records as
// a documented, versioned JSON document that hosts and review tools can read
// without scraping text output. The format is described field by field in
// docs/history-export.md, which also states the compatibility rule.
//
// The document is a PROJECTION with a fixed field list, not a dump of mgit's
// internal records: a field added to an internal type does not appear here
// until it is added on purpose. Left out by design: commit metadata, signatures
// and session ids, file contents and hunks, absolute host paths, and the audit
// details other than mgit's own squash line. Step messages ARE carried as
// recorded, so a rollback's reason appears in its revert step's message.
//
// Refs: MGIT-297
package historyexport

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/hyper-swe/mgit/internal/model"
	"github.com/hyper-swe/mgit/internal/service"
	"github.com/hyper-swe/mgit/internal/store/index"
)

// SchemaVersion is the version this build writes and the only one it reads.
// Rule: removing a field, renaming one, or changing a field's type or meaning
// bumps it; adding an optional field does not.
const SchemaVersion = 1

// ErrUnsupportedVersion is returned for a document whose schema_version this
// reader does not know. ErrNotAnExport is returned for input that is not a
// history export at all.
var (
	ErrUnsupportedVersion = errors.New("unsupported history export schema_version")
	ErrNotAnExport        = errors.New("not a history export")
)

// Document is the whole export.
type Document struct {
	SchemaVersion int           `json:"schema_version"`
	TaskID        string        `json:"task_id"`
	Steps         []Step        `json:"steps"`
	Audit         []AuditRecord `json:"audit"`
}

// Step is one micro-commit of the task, in recorded order.
type Step struct {
	Position    int    `json:"position"`
	CommitID    string `json:"commit_id"`
	ContentHash string `json:"content_hash"`
	ParentID    string `json:"parent_id,omitempty"`
	AgentID     string `json:"agent_id"`
	CreatedAt   string `json:"created_at"`
	SandboxID   string `json:"sandbox_id,omitempty"`
	CommitType  string `json:"commit_type,omitempty"`
	Message     string `json:"message,omitempty"`
	Files       []File `json:"files"`
}

// File is one path a step changed. Hashes are git blob ids; no contents.
type File struct {
	Path      string `json:"path"`
	Operation string `json:"operation"`
	OldHash   string `json:"old_hash,omitempty"`
	NewHash   string `json:"new_hash,omitempty"`
	Binary    bool   `json:"binary,omitempty"`
}

// AuditRecord is one entry of the append-only audit trail for the task.
type AuditRecord struct {
	Timestamp string `json:"timestamp"`
	Operation string `json:"operation"`
	AgentID   string `json:"agent_id"`
	CommitID  string `json:"commit_id,omitempty"`
	Details   string `json:"details,omitempty"`
}

// FromRecord builds a Step from the indexed record and the stored commit. The
// commit may be nil when only the index row is available.
func FromRecord(rec index.CommitRecord, c *model.Commit) Step {
	s := Step{
		Position:    rec.Position,
		CommitID:    rec.CommitHash,
		ContentHash: rec.ContentHash,
		AgentID:     rec.AgentID,
		CreatedAt:   rec.CreatedAt,
		Files:       []File{},
	}
	if rec.SandboxID != nil {
		s.SandboxID = *rec.SandboxID
	}
	if c == nil {
		return s
	}
	s.ParentID = c.ParentID
	s.CommitType = string(c.CommitType)
	s.Message = c.Message
	for _, d := range c.FileDiffs {
		s.Files = append(s.Files, File{
			Path: d.Path, Operation: string(d.Operation), OldHash: d.OldHash, NewHash: d.NewHash, Binary: d.BinaryDiff,
		})
	}
	return s
}

// FromAudit builds an AuditRecord. Details is kept only for the operation whose
// text mgit generates itself (squash); other details can carry user-supplied
// text, such as a rollback reason, and are not repeated here (the reason is
// still in the revert step's message).
func FromAudit(e service.AuditEntry) AuditRecord {
	r := AuditRecord{Timestamp: e.Timestamp, Operation: string(e.Operation), AgentID: e.AgentID, CommitID: e.CommitID}
	if e.Operation == service.AuditSquash {
		r.Details = e.Details
	}
	return r
}

// Build assembles a Document; nil lists become empty arrays.
func Build(taskID string, steps []Step, audit []AuditRecord) Document {
	if steps == nil {
		steps = []Step{}
	}
	if audit == nil {
		audit = []AuditRecord{}
	}
	return Document{SchemaVersion: SchemaVersion, TaskID: taskID, Steps: steps, Audit: audit}
}

// Marshal renders the document as indented JSON ending in a newline. The same
// input always yields the same bytes.
func Marshal(d Document) ([]byte, error) {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	enc.SetIndent("", "  ")
	if err := enc.Encode(d); err != nil {
		return nil, fmt.Errorf("marshal history export: %w", err)
	}
	return buf.Bytes(), nil
}

// Parse reads an export. It reads schema_version first and refuses a missing or
// unknown version before looking at anything else; fields it does not know
// within the supported version are ignored.
func Parse(data []byte) (Document, error) {
	var probe struct {
		SchemaVersion *int `json:"schema_version"`
	}
	if err := json.Unmarshal(data, &probe); err != nil {
		return Document{}, fmt.Errorf("%w: %v", ErrNotAnExport, err)
	}
	if probe.SchemaVersion == nil {
		return Document{}, fmt.Errorf("%w: no schema_version field", ErrNotAnExport)
	}
	if *probe.SchemaVersion != SchemaVersion {
		return Document{}, fmt.Errorf("%w: the document is version %d and this reader supports %d", ErrUnsupportedVersion, *probe.SchemaVersion, SchemaVersion)
	}
	var d Document
	if err := json.Unmarshal(data, &d); err != nil {
		return Document{}, fmt.Errorf("%w: %v", ErrNotAnExport, err)
	}
	return d, nil
}
