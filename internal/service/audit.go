package service

import (
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"time"
)

// AuditOpType identifies the type of audited operation.
// Refs: FR-12
type AuditOpType string

// Supported audit operation types.
const (
	AuditCreateCommit AuditOpType = "CREATE_COMMIT"
	AuditSquash       AuditOpType = "SQUASH"
	AuditRollback     AuditOpType = "ROLLBACK"
	AuditBranchCreate AuditOpType = "BRANCH_CREATE"
	AuditBranchMerge  AuditOpType = "BRANCH_MERGE"
)

// AuditEntry represents a single audit log record.
// Refs: FR-12
type AuditEntry struct {
	Timestamp string      `json:"timestamp"`
	Operation AuditOpType `json:"operation"`
	AgentID   string      `json:"agent_id"`
	TaskID    string      `json:"task_id"`
	CommitID  string      `json:"commit_id,omitempty"`
	Details   string      `json:"details,omitempty"`
}

// AuditFilters constrains audit log queries.
type AuditFilters struct {
	TaskID    string
	AgentID   string
	Operation AuditOpType
	Since     string // RFC3339
	Until     string // RFC3339
}

// AuditService manages the append-only audit log.
// The audit log is a line-delimited JSON file that is only appended to.
// Refs: FR-12, MGIT-3.2.3
type AuditService struct {
	logPath string
	clock   func() time.Time
}

// NewAuditService creates an AuditService writing to the given log path.
func NewAuditService(logPath string, clock func() time.Time) *AuditService {
	return &AuditService{
		logPath: logPath,
		clock:   clock,
	}
}

// LogOperation appends an audit entry to the log file.
// The file is opened with O_APPEND|O_CREATE|O_WRONLY for append-only safety.
// Refs: FR-12
func (s *AuditService) LogOperation(entry AuditEntry) error {
	if entry.Timestamp == "" {
		entry.Timestamp = s.clock().UTC().Format(time.RFC3339)
	}

	data, err := json.Marshal(entry)
	if err != nil {
		return fmt.Errorf("marshal audit entry: %w", err)
	}
	data = append(data, '\n')

	// O_APPEND ensures atomic append even with concurrent writers
	f, err := os.OpenFile(s.logPath, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return fmt.Errorf("open audit log: %w", err)
	}
	defer f.Close() //nolint:errcheck // best-effort close after write

	if _, err := f.Write(data); err != nil {
		return fmt.Errorf("write audit log: %w", err)
	}
	return nil
}

// GetAuditLog reads and filters the audit log.
// Refs: FR-12
func (s *AuditService) GetAuditLog(filters AuditFilters) ([]AuditEntry, error) {
	data, err := os.ReadFile(s.logPath) //nolint:gosec // path is internal config, not user input
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("read audit log: %w", err)
	}

	var entries []AuditEntry
	for _, line := range strings.Split(string(data), "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		var entry AuditEntry
		if err := json.Unmarshal([]byte(line), &entry); err != nil {
			continue // skip malformed lines
		}
		if matchesFilters(entry, filters) {
			entries = append(entries, entry)
		}
	}

	return entries, nil
}

// ExportAuditLog exports the audit log in JSON format.
// Refs: FR-12
func (s *AuditService) ExportAuditLog(filters AuditFilters) ([]byte, error) {
	entries, err := s.GetAuditLog(filters)
	if err != nil {
		return nil, err
	}
	return json.MarshalIndent(entries, "", "  ")
}

// matchesFilters checks if an entry passes the given filters.
func matchesFilters(entry AuditEntry, f AuditFilters) bool {
	if f.TaskID != "" && entry.TaskID != f.TaskID {
		return false
	}
	if f.AgentID != "" && entry.AgentID != f.AgentID {
		return false
	}
	if f.Operation != "" && entry.Operation != f.Operation {
		return false
	}
	return true
}
