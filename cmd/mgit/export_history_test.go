package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/hyper-swe/mgit/internal/historyexport"
)

// TestExportFormatHistory_IsVersionedAndCarriesStepsAndAudit drives the real
// verb: two commits, then the history export. The document must parse with the
// reader, name its schema version, list both steps in order with their files,
// and carry the commit audit records. Refs: MGIT-297
func TestExportFormatHistory_IsVersionedAndCarriesStepsAndAudit(t *testing.T) {
	dir := t.TempDir()
	t.Chdir(dir)
	require.NoError(t, runCLI(t, "init", "--no-agent-files"))
	const taskID = "EXP-297"
	require.NoError(t, os.WriteFile(filepath.Join(dir, "a.txt"), []byte("one\n"), 0o600))
	require.NoError(t, runCLI(t, "commit", "-a", "--task-id", taskID, "-m", "add a"))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "a.txt"), []byte("two\n"), 0o600))
	require.NoError(t, runCLI(t, "commit", "-a", "--task-id", taskID, "-m", "edit a"))

	stdout, stderr, err := runCLICap(t, "export", "--task-id", taskID, "--format", "history")
	require.NoError(t, err, stderr)

	doc, err := historyexport.Parse([]byte(stdout))
	require.NoError(t, err)
	assert.Equal(t, historyexport.SchemaVersion, doc.SchemaVersion)
	assert.Equal(t, taskID, doc.TaskID)
	require.Len(t, doc.Steps, 2)
	assert.Equal(t, 0, doc.Steps[0].Position, "positions are the index's, counted from 0")
	assert.Equal(t, 1, doc.Steps[1].Position)
	assert.Contains(t, doc.Steps[0].Message, "add a")
	assert.Contains(t, doc.Steps[1].Message, "edit a")
	require.NotEmpty(t, doc.Steps[1].Files)
	assert.Equal(t, "a.txt", doc.Steps[1].Files[0].Path)
	assert.Equal(t, doc.Steps[0].CommitID, doc.Steps[1].ParentID)
	assert.NotEmpty(t, doc.Audit, "the commit audit records ride in the export")
	assert.NotContains(t, stdout, dir, "no host path may appear in the export")
}

// TestExportFormatJSON_StaysTheBareCommitArray keeps the earlier, unversioned
// formats exactly as they were: the versioned document is a new format, not a
// change to json. Refs: MGIT-297
func TestExportFormatJSON_StaysTheBareCommitArray(t *testing.T) {
	dir := t.TempDir()
	t.Chdir(dir)
	require.NoError(t, runCLI(t, "init", "--no-agent-files"))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "a.txt"), []byte("one\n"), 0o600))
	require.NoError(t, runCLI(t, "commit", "-a", "--task-id", "EXP-298", "-m", "add a"))

	stdout, _, err := runCLICap(t, "export", "--task-id", "EXP-298", "--format", "json")
	require.NoError(t, err)
	assert.True(t, strings.HasPrefix(strings.TrimSpace(stdout), "["), "json stays an array")
	var rows []map[string]any
	require.NoError(t, json.Unmarshal([]byte(stdout), &rows))
	assert.NotContains(t, rows[0], "schema_version")
}
