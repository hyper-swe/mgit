package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// THE PRODUCT NAMES THE AGENT TOOLS IT WORKS WITH; IT NEVER NAMES THE TOOLS
// THAT BUILD IT (the owner's rule, 2026-09-24). The Agent Documentation
// Generation tickets name the tools and instruction files that feature
// writes for. With the committed lists, report-only over the committed
// board finds nothing in their prompts: integration names are not hits.
// Refs: MGIT-242.1
func TestBoard_TheDocsGeneratorPromptsCarryNoHit(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join("..", "..", ".mtix", "tasks.json"))
	require.NoError(t, err)
	vs, err := boardVersions(raw)
	require.NoError(t, err)
	l, err := loadLists(filepath.Join("..", "..", termsFile), filepath.Join("..", "..", namesFile))
	require.NoError(t, err)
	var prompts []version
	for _, v := range vs {
		id, field, _ := strings.Cut(v.Field, " ")
		if (id == "MGIT-7" || strings.HasPrefix(id, "MGIT-7.")) && field == "prompt" {
			prompts = append(prompts, v)
		}
	}
	require.GreaterOrEqual(t, len(prompts), 15, "the feature's prompts are on the board; an empty selection would pass vacuously")
	for _, h := range judge(prompts, l, cutoff) {
		t.Errorf("%s in %s: an integration name must not be a hit", h.label(), h.Field)
	}
}

// A BUILDER IDENTITY IN AN ASSIGNEE FIELD IS STILL A HIT, while a neutral
// role name there is not, and a prompt naming the instruction file a
// feature writes is not. Refs: MGIT-242.1
func TestBoard_ABuilderIdentityInAnAssigneeIsAHit(t *testing.T) {
	board := `{"nodes":[` +
		`{"id":"MGIT-1","title":"t","prompt":"write AGENTS.md for the agent","assignee":"` + testName + `","updated_at":"2026-09-24T11:00:00Z"},` +
		`{"id":"MGIT-2","title":"t","assignee":"housekeeping","updated_at":"2026-09-24T11:00:00Z"}]}`
	path := filepath.Join(t.TempDir(), "tasks.json")
	require.NoError(t, os.WriteFile(path, []byte(board), 0o600))
	var out bytes.Buffer
	code := checkBoard(path, testLists(), &out)
	assert.Equal(t, 0, code, "the board mode reports and never gates:\n%s", out.String())
	assert.Contains(t, out.String(), "prtext: listed name in MGIT-1 assignee")
	assert.Equal(t, 1, strings.Count(out.String(), "prtext: listed "), "only the builder identity is a hit:\n%s", out.String())
	assert.NotContains(t, strings.ToLower(out.String()), "quokka", "the hit never names the identity")
	assert.Contains(t, lastLine(out.String()), "1 hits")
}

func TestBoard_WhatItCannotReadIsNotChecked(t *testing.T) {
	dir := t.TempDir()
	for name, body := range map[string]string{"not json": "{", "no nodes": `{"nodes":[]}`} {
		t.Run(name, func(t *testing.T) {
			path := filepath.Join(dir, strings.ReplaceAll(name, " ", "-")+".json")
			require.NoError(t, os.WriteFile(path, []byte(body), 0o600))
			var out bytes.Buffer
			assert.Equal(t, 2, checkBoard(path, testLists(), &out))
			assert.Contains(t, out.String(), "prtext: NOT CHECKED")
		})
	}
}
