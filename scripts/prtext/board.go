package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
)

// boardNode is the part of the tracker's export (.mtix/tasks.json) the
// board mode reads: every text field a node carries, and the assignee,
// where a builder identity would sit.
type boardNode struct {
	ID          string `json:"id"`
	Title       string `json:"title"`
	Description string `json:"description"`
	Prompt      string `json:"prompt"`
	Acceptance  string `json:"acceptance"`
	Assignee    string `json:"assignee"`
	UpdatedAt   string `json:"updated_at"`
}

// boardVersions turns each node's fields into versions named by node and
// field, so a hit reads "MGIT-7.1 prompt".
func boardVersions(raw []byte) ([]version, error) {
	var b struct {
		Nodes []boardNode `json:"nodes"`
	}
	if err := json.Unmarshal(raw, &b); err != nil {
		return nil, fmt.Errorf("the board is not the tracker's export: %w", err)
	}
	if len(b.Nodes) == 0 {
		return nil, errors.New("the board carries no nodes")
	}
	var vs []version
	for _, n := range b.Nodes {
		at := parseTime(n.UpdatedAt)
		for _, f := range []struct{ name, text string }{
			{"title", n.Title}, {"description", n.Description}, {"prompt", n.Prompt},
			{"acceptance", n.Acceptance}, {"assignee", n.Assignee},
		} {
			if f.text != "" {
				vs = append(vs, version{Field: n.ID + " " + f.name, Rev: "as exported", At: at, Text: f.text})
			}
		}
	}
	return vs, nil
}

// checkBoard reports every hit on the board by node and field, never the
// word, and never gates: the board is the tracker's, and its builder
// identities are emptied by its own export (MGIT-241). Exit 2 when the
// board cannot be read, else 0. Refs: MGIT-242.1
func checkBoard(path string, l lists, w io.Writer) int {
	raw, err := os.ReadFile(path) //nolint:gosec // G304: the board path is the operator's argument
	if err == nil {
		var vs []version
		if vs, err = boardVersions(raw); err == nil {
			hits := judge(vs, l, cutoff)
			for _, h := range hits {
				fmt.Fprintf(w, "prtext: %s in %s\n", h.label(), h.Field)
			}
			fmt.Fprintf(w, "prtext: board (report only) — %d text fields read, %d hits\n", len(vs), len(hits))
			return 0
		}
	}
	fmt.Fprintf(w, "prtext: NOT CHECKED — the board %s: %v\n", path, err)
	return 2
}
