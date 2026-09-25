// boardexport writes the tracked board file (.mtix/tasks.json) with no
// agent identities: every node's assignee is blank, no creator names who
// filed it (MGIT-264), and the agents and sessions sections are empty. The board is this repository's public ledger
// and is regenerated whole on every export, so anything it carries is
// republished each time; it had been publishing the working identities of
// the agents that claimed tickets (MGIT-241).
//
// The tracker (mtix) writes the file and verifies it on import: a SHA-256
// over the canonical JSON of its nodes and dependencies. It has no
// redaction option. So this tool mirrors the tracker's export schema field
// for field, and it PROVES the mirror before changing anything: decoding
// and re-encoding must reproduce the input byte for byte, and the checksum
// must verify. A field the tracker adds, or a file already inconsistent,
// is refused rather than rewritten. A node's content_hash covers its title,
// description, prompt, acceptance and labels, never the assignee, so node
// integrity is untouched.
//
// NEVER BESIDE A LIVE DATABASE. The tracker auto-imports tasks.json in
// REPLACE mode on any command when the file differs from its own last
// export, and REPLACE drops everything the file does not carry: the
// database's assignees, agents and annotations. The tool refuses to write a
// board next to a .mtix/data directory. Board exports are cut in a fresh
// worktree, never in the checkout that holds the database.
//
// Usage:
//
//	go run ./scripts/boardexport [-in .mtix/tasks.json] [-out <same>] [-check]
//
// -check reports what identities remain and exits 1 if any do (or if the
// board cannot be verified), changing nothing. Refs: MGIT-241
package main

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"path/filepath"
)

// exportData mirrors the tracker's export (mtix internal/store/sqlite
// ExportData) field for field and in order, so encoding it reproduces the
// file byte for byte.
type exportData struct {
	Version       int             `json:"version"`
	SchemaVersion string          `json:"schema_version"`
	ExportedAt    string          `json:"exported_at"`
	MtixVersion   string          `json:"mtix_version"`
	Project       string          `json:"project"`
	Nodes         []exportNode    `json:"nodes"`
	Dependencies  []exportDep     `json:"dependencies"`
	Agents        []exportAgent   `json:"agents"`
	Sessions      []exportSession `json:"sessions"`
	NodeCount     int             `json:"node_count"`
	Checksum      string          `json:"checksum"`
}

type exportNode struct {
	ID          string  `json:"id"`
	ParentID    string  `json:"parent_id"`
	Depth       int     `json:"depth"`
	Seq         int     `json:"seq"`
	Project     string  `json:"project"`
	Title       string  `json:"title"`
	Description string  `json:"description"`
	Prompt      string  `json:"prompt"`
	Acceptance  string  `json:"acceptance"`
	NodeType    string  `json:"node_type"`
	IssueType   string  `json:"issue_type"`
	Priority    int     `json:"priority"`
	Labels      string  `json:"labels"`
	Status      string  `json:"status"`
	Progress    float64 `json:"progress"`
	Assignee    string  `json:"assignee"`
	Creator     string  `json:"creator"`
	AgentState  string  `json:"agent_state"`
	Weight      float64 `json:"weight"`
	ContentHash string  `json:"content_hash"`
	CreatedAt   string  `json:"created_at"`
	UpdatedAt   string  `json:"updated_at"`
	ClosedAt    string  `json:"closed_at,omitempty"`
	DeferUntil  string  `json:"defer_until,omitempty"`
	DeletedAt   string  `json:"deleted_at,omitempty"`
	UID         string  `json:"uid,omitempty"`
}

type exportDep struct {
	FromID    string `json:"from_id"`
	ToID      string `json:"to_id"`
	DepType   string `json:"dep_type"`
	CreatedAt string `json:"created_at"`
}

type exportAgent struct {
	AgentID       string `json:"agent_id"`
	Project       string `json:"project"`
	State         string `json:"state"`
	CurrentNodeID string `json:"current_node_id,omitempty"`
	LastHeartbeat string `json:"last_heartbeat,omitempty"`
}

type exportSession struct {
	ID        string `json:"id"`
	AgentID   string `json:"agent_id"`
	Project   string `json:"project"`
	StartedAt string `json:"started_at"`
	EndedAt   string `json:"ended_at,omitempty"`
	Status    string `json:"status"`
	Summary   string `json:"summary,omitempty"`
}

// encode writes a board exactly as the tracker does.
func encode(d *exportData) ([]byte, error) {
	return json.MarshalIndent(d, "", "  ")
}

// checksum is the tracker's integrity hash: SHA-256 over the compact JSON
// of the nodes and dependencies, under the keys "nodes" and "deps".
func checksum(d *exportData) (string, error) {
	canonical := struct {
		Nodes []exportNode `json:"nodes"`
		Deps  []exportDep  `json:"deps"`
	}{Nodes: d.Nodes, Deps: d.Dependencies}
	b, err := json.Marshal(canonical)
	if err != nil {
		return "", fmt.Errorf("encode for checksum: %w", err)
	}
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:]), nil
}

// load reads a board and proves the mirror reproduces it before anything
// is changed.
func load(raw []byte) (*exportData, error) {
	var d exportData
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&d); err != nil {
		return nil, fmt.Errorf("the board does not match the export schema this tool mirrors: %w", err)
	}
	again, err := encode(&d)
	if err != nil {
		return nil, err
	}
	if !bytes.Equal(again, raw) {
		return nil, errors.New("re-encoding the board does not reproduce it byte for byte: the tracker's export format changed, so update the mirror before redacting")
	}
	sum, err := checksum(&d)
	if err != nil {
		return nil, err
	}
	if sum != d.Checksum {
		return nil, fmt.Errorf("the board's checksum does not verify (computed %s, the file says %s): refusing to rewrite an inconsistent board", sum, d.Checksum)
	}
	return &d, nil
}

// identities counts what a board still names: assigned nodes, agent rows
// and session rows.
func identities(d *exportData) (assignees, agents, sessions int) {
	for _, n := range d.Nodes {
		if n.Assignee != "" {
			assignees++
		}
	}
	return assignees, len(d.Agents), len(d.Sessions)
}

// creatorNamesSomeone reports whether a node's creator is an identity rather
// than the interface it was filed through. The tracker writes "cli" or "mcp"
// by default, but takes --assign or the filer's author identity
// (MTIX_AUTHOR_ID) when one is given, so any other value names who filed it.
// Refs: MGIT-264
func creatorNamesSomeone(creator string) bool {
	switch creator {
	case "", "cli", "mcp":
		return false
	}
	return true
}

// namedCreators counts the nodes whose creator names someone.
func namedCreators(d *exportData) int {
	named := 0
	for _, n := range d.Nodes {
		if creatorNamesSomeone(n.Creator) {
			named++
		}
	}
	return named
}

// redact blanks every assignee and every creator that names someone,
// empties the agents and sessions sections, and recomputes the checksum. It
// returns how many assignees it blanked. A node's content_hash does not cover
// its creator, so blanking one leaves node integrity untouched.
func redact(d *exportData) int {
	blanked := 0
	for i := range d.Nodes {
		if d.Nodes[i].Assignee != "" {
			d.Nodes[i].Assignee = ""
			blanked++
		}
		if creatorNamesSomeone(d.Nodes[i].Creator) {
			d.Nodes[i].Creator = ""
		}
	}
	d.Agents, d.Sessions = nil, nil
	// The inputs are strings and numbers that just decoded, so encoding
	// them cannot fail; a failure would surface in the load of the output.
	d.Checksum, _ = checksum(d)
	return blanked
}

// rewrite applies redactFn to the board and encodes it, then proves the
// result loads (reproducible, checksum verifying) before handing the bytes
// on: a redaction that leaves the board inconsistent is refused with
// nothing to write, never published as a board the tracker would reject.
func rewrite(d *exportData, redactFn func(*exportData) int) ([]byte, int, error) {
	blanked := redactFn(d)
	body, err := encode(d)
	if err != nil {
		return nil, 0, err
	}
	if _, err := load(body); err != nil {
		return nil, 0, fmt.Errorf("the redacted board does not verify, nothing written: %w", err)
	}
	return body, blanked, nil
}

// run redacts the board at in into out, or with check only reports.
func run(in, out string, check bool) error {
	raw, err := os.ReadFile(in) //nolint:gosec // G304: the board path is the operator's argument
	if err != nil {
		return fmt.Errorf("read the board: %w", err)
	}
	d, err := load(raw)
	if err != nil {
		return err
	}
	a, g, s := identities(d)
	c := namedCreators(d)
	if check {
		if a+g+s+c > 0 {
			return fmt.Errorf("the board names identities: %d assigned nodes, %d creators naming someone, %d agent rows, %d session rows", a, c, g, s)
		}
		fmt.Println("boardexport: the board names no identities and its checksum verifies")
		return nil
	}
	if info, statErr := os.Stat(filepath.Join(filepath.Dir(out), "data")); statErr == nil && info.IsDir() {
		return fmt.Errorf("REFUSING: %s sits beside a live tracker database (%s): the tracker would auto-import this board in REPLACE mode and lose the database's assignees, agents and annotations. Cut board exports in a fresh worktree",
			out, filepath.Join(filepath.Dir(out), "data"))
	}
	body, blanked, err := rewrite(d, redact)
	if err != nil {
		return err
	}
	tmp := out + ".tmp"
	if err := os.WriteFile(tmp, body, 0o644); err != nil { //nolint:gosec // G306: the tracked board is a public, world-readable file
		return fmt.Errorf("write the board: %w", err)
	}
	if err := os.Rename(tmp, out); err != nil {
		return fmt.Errorf("replace the board: %w", err)
	}
	fmt.Printf("boardexport: blanked %d assignees and %d creators, removed %d agent rows and %d session rows; checksum %s\n", blanked, c, g, s, d.Checksum)
	return nil
}

func main() {
	in := flag.String("in", filepath.Join(".mtix", "tasks.json"), "the board file to read")
	out := flag.String("out", "", "where to write the redacted board (default: -in)")
	check := flag.Bool("check", false, "report what identities remain and fail if any do; change nothing")
	flag.Parse()
	if *out == "" {
		*out = *in
	}
	if err := run(*in, *out, *check); err != nil {
		fmt.Fprintln(os.Stderr, "boardexport:", err)
		os.Exit(1)
	}
}
