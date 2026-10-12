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
	for _, n := range d.Nodes {
		assert.Contains(t, []string{"", "cli", "mcp"}, n.Creator,
			"%s names who filed it: a creator is the interface (cli, mcp), never an identity (MGIT-264)", n.ID)
	}
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

// Each identity count stands on its own. The committed-board test relies on
// the agent-row and session-row counts to keep either section from coming
// back by itself, so a board carrying ONLY an agent row, or ONLY a session
// row, must still count as naming an identity, and -check must fail on it.
// Refs: MGIT-241
func TestIdentities_EachSectionCountsWithoutAnAssignee(t *testing.T) {
	tests := []struct {
		name                      string
		strip                     func(d *exportData)
		wantAssignees, wantAgents int
		wantSessions              int
	}{
		{"only_an_agent_row", func(d *exportData) { d.Nodes[0].Assignee, d.Sessions = "", nil }, 0, 1, 0},
		{"only_a_session_row", func(d *exportData) { d.Nodes[0].Assignee, d.Agents = "", nil }, 0, 0, 1},
		{"only_an_assignee", func(d *exportData) { d.Agents, d.Sessions = nil, nil }, 1, 0, 0},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			d := fixture(t)
			tt.strip(d)
			sum, err := checksum(d)
			require.NoError(t, err)
			d.Checksum = sum

			a, g, s := identities(d)
			assert.Equal(t, []int{tt.wantAssignees, tt.wantAgents, tt.wantSessions}, []int{a, g, s})

			raw, err := encode(d)
			require.NoError(t, err)
			board := filepath.Join(t.TempDir(), "tasks.json")
			require.NoError(t, os.WriteFile(board, raw, 0o600))
			err = run(board, board, true)
			require.Error(t, err, "-check fails on a board that names any identity")
			assert.Contains(t, err.Error(), "names identities")
		})
	}
}

// THE OUTPUT IS VERIFIED BEFORE IT IS WRITTEN. A redaction that leaves the
// board inconsistent (here, one that blanks an assignee and forgets the
// checksum) must be refused with nothing written, never published as a
// board the tracker would reject on import. Refs: MGIT-241
func TestRewrite_ARedactionThatBreaksTheBoardIsRefused(t *testing.T) {
	forgetful := func(d *exportData) int {
		d.Nodes[0].Assignee, d.Agents, d.Sessions = "", nil, nil
		return 1
	}
	body, n, err := rewrite(fixture(t), forgetful)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "does not verify")
	assert.Nil(t, body, "no bytes are handed on to be written")
	assert.Zero(t, n)

	body, n, err = rewrite(fixture(t), redact)
	require.NoError(t, err)
	assert.Equal(t, 1, n)
	_, err = load(body)
	require.NoError(t, err, "the real redaction's output verifies")
}

// A CREATOR CAN NAME SOMEONE. The tracker records a node's creator from
// `create --assign`, or from the filer's author identity (MTIX_AUTHOR_ID)
// when one is set, not only as the interface ("cli", "mcp"). A ticket filed
// that way carried a session name onto the board while -check reported "the
// board names no identities". A board whose ONLY identity is a creator must
// fail -check, and the rewrite must blank it while keeping the interface
// names and a board that verifies. Refs: MGIT-264, MGIT-241
func TestRun_ACreatorThatNamesSomeoneIsAnIdentity(t *testing.T) {
	d := fixture(t)
	d.Nodes[0].Assignee, d.Agents, d.Sessions = "", nil, nil
	d.Nodes[0].Creator = "some-session-35"
	sum, err := checksum(d)
	require.NoError(t, err)
	d.Checksum = sum
	raw, err := encode(d)
	require.NoError(t, err)
	board := filepath.Join(t.TempDir(), "tasks.json")
	require.NoError(t, os.WriteFile(board, raw, 0o600))

	err = run(board, board, true)
	require.Error(t, err, "-check fails on a board whose only identity is a creator")
	assert.Contains(t, err.Error(), "names identities")

	require.NoError(t, run(board, board, false))
	after, err := os.ReadFile(board) //nolint:gosec // G304: test-only; board is this test's own temp file
	require.NoError(t, err)
	got, err := load(after)
	require.NoError(t, err, "the rewritten board is reproducible and its checksum verifies")
	assert.Empty(t, got.Nodes[0].Creator, "the creator that named someone is blanked")
	assert.Equal(t, "mcp", got.Nodes[1].Creator, "an interface name is not an identity and stays")
	assert.Equal(t, d.Nodes[0].ContentHash, got.Nodes[0].ContentHash, "content is untouched")
	require.NoError(t, run(board, board, true), "after the rewrite, -check passes")
}
