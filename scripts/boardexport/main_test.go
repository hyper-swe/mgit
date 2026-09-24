package main

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// THE PUBLIC BOARD CARRIES NO AGENT IDENTITIES. The tracked board is this
// repository's public ledger, and it is regenerated whole on every export.
// It published the working identities of the agents that claimed tickets:
// node assignees and an agents section naming tools, models and sessions.
// The committed file must carry none, and must still verify, so the tracker
// can import it. Refs: MGIT-241
func TestTheCommittedBoardCarriesNoAgentIdentities(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join("..", "..", ".mtix", "tasks.json"))
	require.NoError(t, err)
	d, err := load(raw)
	require.NoError(t, err, "the committed board must be reproducible and verify")
	assignees, agents, sessions := identities(d)
	assert.Zero(t, assignees, "nodes still name who claimed them")
	assert.Zero(t, agents, "the agents section still lists identities")
	assert.Zero(t, sessions, "the sessions section still lists identities")
}

// fixture is a small board as the tracker writes it.
func fixture(t *testing.T) *exportData {
	t.Helper()
	d := &exportData{
		Version: 1, SchemaVersion: "1.0.0", ExportedAt: "2026-09-24T10:00:00Z", Project: "",
		Nodes: []exportNode{
			{ID: "MGIT-1", Depth: 0, Seq: 1, Project: "MGIT", Title: "one <&> — ☃", NodeType: "story",
				Priority: 2, Labels: "a,b", Status: "open", Progress: 0.5, Assignee: "some-agent-01",
				Creator: "cli", Weight: 1, ContentHash: "h1", CreatedAt: "c", UpdatedAt: "u", UID: "x1"},
			{ID: "MGIT-2", Depth: 0, Seq: 2, Project: "MGIT", Title: "two", NodeType: "bug",
				Priority: 3, Status: "done", Progress: 1, Creator: "mcp", Weight: 1, ContentHash: "h2",
				CreatedAt: "c", UpdatedAt: "u", ClosedAt: "z"},
		},
		Dependencies: []exportDep{{FromID: "MGIT-2", ToID: "MGIT-1", DepType: "blocks", CreatedAt: "c"}},
		Agents:       []exportAgent{{AgentID: "some-agent-01", Project: "MGIT", State: "idle"}},
		Sessions:     []exportSession{{ID: "s1", AgentID: "some-agent-01", Project: "MGIT", StartedAt: "a", Status: "ended"}},
		NodeCount:    2,
	}
	sum, err := checksum(d)
	require.NoError(t, err)
	d.Checksum = sum
	return d
}

func TestLoad_ReproducesTheBoardAndRefusesWhatItCannotReproduce(t *testing.T) {
	good, err := encode(fixture(t))
	require.NoError(t, err)
	_, err = load(good)
	require.NoError(t, err, "a board the tracker wrote loads")

	tests := []struct {
		name, want string
		raw        []byte
	}{
		{"a field the mirror does not know", "schema", []byte(`{"version":1,"brand_new":true}`)},
		{"the same content, re-indented", "byte for byte", append([]byte(" "), good...)},
		{"a checksum that does not verify", "checksum",
			func() []byte {
				d := fixture(t)
				d.Checksum = "0000"
				b, _ := encode(d)
				return b
			}()},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := load(tt.raw)
			require.Error(t, err)
			assert.Contains(t, err.Error(), tt.want)
		})
	}
}

func TestRedact_BlanksIdentitiesAndKeepsTheBoardVerifiable(t *testing.T) {
	d := fixture(t)
	hashes := []string{d.Nodes[0].ContentHash, d.Nodes[1].ContentHash}

	n := redact(d)

	assert.Equal(t, 1, n, "one node named an assignee")
	a, g, s := identities(d)
	assert.Zero(t, a+g+s, "no identity is left")
	assert.Equal(t, hashes, []string{d.Nodes[0].ContentHash, d.Nodes[1].ContentHash}, "content is untouched")
	raw, err := encode(d)
	require.NoError(t, err)
	_, err = load(raw)
	require.NoError(t, err, "the redacted board is reproducible and its checksum verifies")
}

// A REDACTED BOARD MUST NEVER SIT BESIDE A LIVE DATABASE. The tracker
// auto-imports tasks.json in REPLACE mode on any command when the file
// differs from its own last export, and REPLACE drops everything the file
// does not carry. Written there, this board would blank the database's
// assignees and agents and drop its annotations. Refs: MGIT-241
func TestRun_RefusesToWriteBesideALiveDatabase(t *testing.T) {
	dir := t.TempDir()
	raw, err := encode(fixture(t))
	require.NoError(t, err)
	board := filepath.Join(dir, "tasks.json")
	require.NoError(t, os.WriteFile(board, raw, 0o600))
	require.NoError(t, os.MkdirAll(filepath.Join(dir, "data"), 0o750))

	err = run(board, board, false)

	require.Error(t, err)
	assert.Contains(t, err.Error(), "database")
	after, readErr := os.ReadFile(board) //nolint:gosec // G304: test-only; board is this test's own temp file
	require.NoError(t, readErr)
	assert.Equal(t, raw, after, "the board beside the database is left untouched")
}

func TestRun_RewritesAndCheckReportsWhatIsLeft(t *testing.T) {
	dir := t.TempDir()
	raw, err := encode(fixture(t))
	require.NoError(t, err)
	board := filepath.Join(dir, "tasks.json")
	require.NoError(t, os.WriteFile(board, raw, 0o600))

	require.Error(t, run(board, board, true), "check mode reports identities as a failure")
	require.NoError(t, run(board, board, false))
	require.NoError(t, run(board, board, true), "after the rewrite, check mode passes")
}
