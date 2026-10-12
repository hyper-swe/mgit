package main

import (
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// mgit's agent instructions follow the one-entry-point pattern: mgit init
// writes only its own file, MGIT.md, plus one pointer line in CLAUDE.md and
// AGENTS.md. When HYPERSWE.md exists, the other tool owns the entry point
// and links MGIT.md itself, so mgit adds no pointer. Refs: HSWE-19.3

const agentsSentence = "This project uses mgit for task-tagged commits and sandboxed work; see MGIT.md for instructions."

func initAt(t *testing.T, dir string) {
	t.Helper()
	require.NoError(t, runCLI(t, "init", "--path", dir))
}

func read(t *testing.T, dir, name string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(dir, name)) //nolint:gosec // test fixture path under t.TempDir()
	require.NoError(t, err)
	return string(b)
}

func write(t *testing.T, dir, name, body string) {
	t.Helper()
	require.NoError(t, os.WriteFile(filepath.Join(dir, name), []byte(body), 0o600))
}

func has(dir, name string) bool {
	_, err := os.Stat(filepath.Join(dir, name))
	return err == nil
}

func TestInitWritesMgitMdAndOnePointerLineEach(t *testing.T) {
	dir := t.TempDir()
	write(t, dir, "CLAUDE.md", "# Mine\nrules\n")
	initAt(t, dir)

	assert.Equal(t, "# Mine\nrules\n@MGIT.md\n", read(t, dir, "CLAUDE.md"))
	assert.Equal(t, agentsSentence+"\n", read(t, dir, "AGENTS.md"), "a missing AGENTS.md is created holding only the pointer")
	md := read(t, dir, "MGIT.md")
	for _, want := range []string{"mgit commit", "--task-id", "mgit squash", "mgit verify"} {
		assert.Contains(t, md, want, "MGIT.md teaches the loop")
	}
}

// A second run adds nothing. `mgit init` itself refuses an initialized
// directory, so the writer is run twice directly as well.
func TestInitEntryIsIdempotent(t *testing.T) {
	dir := t.TempDir()
	initAt(t, dir)
	first := map[string]string{}
	for _, f := range []string{"CLAUDE.md", "AGENTS.md", "MGIT.md"} {
		first[f] = read(t, dir, f)
	}
	require.Error(t, runCLI(t, "init", "--path", dir), "an initialized directory is refused, as before")
	require.NoError(t, writeAgentEntry(dir, io.Discard))
	require.NoError(t, writeAgentEntry(dir, io.Discard))
	for f, want := range first {
		assert.Equal(t, want, read(t, dir, f), f)
	}
	assert.Equal(t, 1, strings.Count(read(t, dir, "CLAUDE.md"), "@MGIT.md"))
	assert.Equal(t, 1, strings.Count(read(t, dir, "AGENTS.md"), agentsSentence))
}

func TestInitKeepsAPointerTheUserPlaced(t *testing.T) {
	dir := t.TempDir()
	mine := "# Mine\n@MGIT.md\nmy rules after it\n"
	write(t, dir, "CLAUDE.md", mine)
	initAt(t, dir)
	assert.Equal(t, mine, read(t, dir, "CLAUDE.md"), "an existing pointer is not duplicated, moved or removed")
}

func TestInitLeavesTheEntryPointToHyperswe(t *testing.T) {
	dir := t.TempDir()
	write(t, dir, "HYPERSWE.md", "# HyperSWE\n")
	write(t, dir, "CLAUDE.md", "# Mine\n@HYPERSWE.md\n")
	initAt(t, dir)

	assert.Equal(t, "# Mine\n@HYPERSWE.md\n", read(t, dir, "CLAUDE.md"), "no mgit pointer beside HyperSWE's")
	assert.False(t, has(dir, "AGENTS.md"), "no AGENTS.md is created for a pointer HyperSWE links itself")
	assert.True(t, has(dir, "MGIT.md"), "mgit still writes its own file")
	assert.Equal(t, "# HyperSWE\n", read(t, dir, "HYPERSWE.md"), "mgit never writes into HYPERSWE.md")
}

func TestInitNeverRemovesAPointerLineWhenHyperswePresent(t *testing.T) {
	dir := t.TempDir()
	write(t, dir, "HYPERSWE.md", "# HyperSWE\n")
	mine := "# Mine\n@MGIT.md\n"
	write(t, dir, "CLAUDE.md", mine)
	initAt(t, dir)
	assert.Equal(t, mine, read(t, dir, "CLAUDE.md"), "a line the user placed stays")
}

func TestInitAddsThePointerWithoutTrailingNewline(t *testing.T) {
	dir := t.TempDir()
	write(t, dir, "CLAUDE.md", "# Mine\nrules")
	initAt(t, dir)
	assert.Equal(t, "# Mine\nrules\n@MGIT.md", read(t, dir, "CLAUDE.md"))
}

func TestInitRewritesItsOwnFileOnly(t *testing.T) {
	dir := t.TempDir()
	write(t, dir, "MGIT.md", "stale\n")
	initAt(t, dir)
	assert.NotEqual(t, "stale\n", read(t, dir, "MGIT.md"), "MGIT.md is mgit's to refresh")
}

func TestInitCanSkipTheAgentFiles(t *testing.T) {
	dir := t.TempDir()
	require.NoError(t, runCLI(t, "init", "--path", dir, "--no-agent-files"))
	for _, f := range []string{"MGIT.md", "CLAUDE.md", "AGENTS.md"} {
		assert.False(t, has(dir, f), f)
	}
}
