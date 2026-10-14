package historyexport

import (
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/hyper-swe/mgit/internal/model"
	"github.com/hyper-swe/mgit/internal/service"
	"github.com/hyper-swe/mgit/internal/store/index"
)

func fixture() Document {
	sandbox := "sbx-1"
	steps := []Step{
		FromRecord(index.CommitRecord{TaskID: "MGIT-1", CommitHash: "c1", ContentHash: "h1", AgentID: "agent-a", Position: 1, CreatedAt: "2026-10-01T10:00:00Z", SandboxID: &sandbox},
			&model.Commit{CommitID: "c1", Message: "[MGIT:MGIT-1] add a.txt", CommitType: model.CommitType("normal"),
				SessionID: "SESSION-SECRET", Signature: "SIG", Metadata: map[string]any{"token": "METADATA-SECRET"},
				CreatedAt: time.Date(2026, 10, 1, 10, 0, 0, 0, time.UTC),
				FileDiffs: []model.FileDiff{{Path: "a.txt", Operation: model.DiffAdded, NewHash: "n1"}}}),
		FromRecord(index.CommitRecord{TaskID: "MGIT-1", CommitHash: "c2", ContentHash: "h2", AgentID: "agent-a", Position: 2, CreatedAt: "2026-10-01T10:05:00Z"},
			&model.Commit{CommitID: "c2", ParentID: "c1", Message: "[MGIT:MGIT-1] edit a.txt", CommitType: model.CommitType("normal"),
				FileDiffs: []model.FileDiff{{Path: "a.txt", Operation: model.DiffModified, OldHash: "n1", NewHash: "n2"}}}),
	}
	audit := []AuditRecord{
		FromAudit(service.AuditEntry{Timestamp: "2026-10-01T10:00:00Z", Operation: service.AuditCreateCommit, AgentID: "agent-a", TaskID: "MGIT-1", CommitID: "c1"}),
		FromAudit(service.AuditEntry{Timestamp: "2026-10-01T11:00:00Z", Operation: service.AuditSquash, AgentID: "agent-a", TaskID: "MGIT-1", CommitID: "c3", Details: "squashed 2 commits"}),
		FromAudit(service.AuditEntry{Timestamp: "2026-10-01T12:00:00Z", Operation: service.AuditRollback, AgentID: "agent-a", TaskID: "MGIT-1", CommitID: "c4", Details: "user text: password=hunter2 in /Users/someone/x"}),
	}
	return Build("MGIT-1", steps, audit)
}

func TestExport_MatchesTheGoldenSchema(t *testing.T) {
	got, err := Marshal(fixture())
	if err != nil {
		t.Fatal(err)
	}
	want, err := os.ReadFile("testdata/history-v1.golden.json")
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, want) {
		t.Fatalf("the v1 export changed; a change to this file is a schema change and follows the compatibility rule in docs/history-export.md\n--- got\n%s\n--- want\n%s", got, want)
	}
}

func TestExport_CarriesNoHostLocalOrSecretFields(t *testing.T) {
	got, err := Marshal(fixture())
	if err != nil {
		t.Fatal(err)
	}
	for _, leak := range []string{"SESSION-SECRET", "METADATA-SECRET", "SIG", "hunter2", "/Users/", "session_id", "metadata", "signature"} {
		if strings.Contains(string(got), leak) {
			t.Errorf("the export contains %q", leak)
		}
	}
}

func TestParse_RoundTripsTheCurrentVersion(t *testing.T) {
	data, err := Marshal(fixture())
	if err != nil {
		t.Fatal(err)
	}
	doc, err := Parse(data)
	if err != nil {
		t.Fatal(err)
	}
	if doc.SchemaVersion != SchemaVersion || len(doc.Steps) != 2 || len(doc.Audit) != 3 {
		t.Fatalf("round trip lost data: %+v", doc)
	}
}

func TestParse_RefusesAVersionItDoesNotKnow(t *testing.T) {
	for _, in := range []string{
		`{"schema_version": 2, "task_id": "X", "steps": [], "audit": []}`,
		`{"schema_version": 0, "task_id": "X"}`,
		`{"schema_version": -1}`,
		`{"task_id": "X", "steps": []}`,
		`{"schema_version": "1"}`,
		`[]`,
		``,
	} {
		_, err := Parse([]byte(in))
		if err == nil {
			t.Errorf("Parse(%q) accepted an unknown or missing version", in)
			continue
		}
		if !errors.Is(err, ErrUnsupportedVersion) && !errors.Is(err, ErrNotAnExport) {
			t.Errorf("Parse(%q) error %v is neither ErrUnsupportedVersion nor ErrNotAnExport", in, err)
		}
	}
	_, err := Parse([]byte(`{"schema_version": 7}`))
	if err == nil || !strings.Contains(err.Error(), "7") || !strings.Contains(err.Error(), "supports 1") {
		t.Errorf("the refusal must name the version it saw and the one it supports, got %v", err)
	}
}

func TestParse_IgnoresFieldsAddedWithinTheSameVersion(t *testing.T) {
	doc, err := Parse([]byte(`{"schema_version": 1, "task_id": "X", "steps": [], "audit": [], "added_later": {"a": 1}}`))
	if err != nil || doc.TaskID != "X" {
		t.Fatalf("an additive field must not break a v1 reader: %v", err)
	}
}

func TestMarshal_IsDeterministicAndEmptyListsAreArrays(t *testing.T) {
	a, _ := Marshal(Build("T-1", nil, nil))
	b, _ := Marshal(Build("T-1", nil, nil))
	if !bytes.Equal(a, b) {
		t.Fatal("two exports of the same input differ")
	}
	var m map[string]json.RawMessage
	if err := json.Unmarshal(a, &m); err != nil || string(m["steps"]) != "[]" || string(m["audit"]) != "[]" {
		t.Fatalf("empty steps and audit must be [], got %s", a)
	}
}
