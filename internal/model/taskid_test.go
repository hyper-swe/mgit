package model

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestTaskID_Parse(t *testing.T) {
	tests := []struct {
		name    string
		input   string
		wantErr bool
	}{
		{"single_level", "MGIT-1", false},
		{"two_levels", "MGIT-1.2", false},
		{"three_levels", "MGIT-1.2.3", false},
		{"different_prefix", "PROJ-4.2.1", false},
		{"large_numbers", "MGIT-99.88.77", false},
		// Broadened real-world forms (MGIT-41).
		{"dash_suffix_probe", "MTIX-30-probe", false},
		{"dot_two_part", "MTIX-30.6", false},
		{"deep_nesting", "MGIT-11.13.5", false},
		{"mixed_dash_dot", "MGIT-11.13.5-probe", false},
		{"underscore_body", "MGIT-1_a", false},
		{"alpha_segment", "MGIT-feature.x", false},
		{"numeric_prefix", "PROJ2-1.2", false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			tid, err := ParseTaskID(tt.input)
			if tt.wantErr {
				assert.Error(t, err)
			} else {
				require.NoError(t, err)
				assert.Equal(t, tt.input, tid.String())
			}
		})
	}
}

// TestTaskID_Parse_BroadenedAccepts confirms the broadened grammar (MGIT-41)
// accepts the real-world ids that mtix and users actually emit. The trigger
// was `mgit worktree add ... --task MTIX-30-probe` being rejected.
func TestTaskID_Parse_BroadenedAccepts(t *testing.T) {
	tests := []struct {
		name  string
		input string
	}{
		{"mtix_dash_probe", "MTIX-30-probe"},
		{"mtix_dot_two_part", "MTIX-30.6"},
		{"mgit_three_part", "MGIT-1.2.3"},
		{"mgit_deep", "MGIT-11.13.5"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			tid, err := ParseTaskID(tt.input)
			require.NoError(t, err, "ParseTaskID(%q) should succeed", tt.input)
			// Round-trips exactly.
			assert.Equal(t, tt.input, tid.String())
			// Derived branch ref must be a clean git ref (no separators/spaces).
			ref := TaskBranchName(tt.input)
			assert.False(t, strings.ContainsAny(ref[len(TaskBranchPrefix):], " \t/\\"),
				"branch ref %q must stay git-ref-safe", ref)
		})
	}
}

// TestTaskID_Parse_RejectsUnsafe confirms ids that would be dangerous as git
// refs, commit-message tags, or SQL params stay rejected (MGIT-41).
func TestTaskID_Parse_RejectsUnsafe(t *testing.T) {
	tests := []struct {
		name  string
		input string
	}{
		{"path_traversal", "../etc"},
		{"slash_path", "a/b"},
		{"backslash_path", "a\\b"},
		{"space", "MTIX 30"},
		{"semicolon_injection", "MTIX-30;rm"},
		{"shell_var", "MTIX-$x"},
		{"empty", ""},
		{"control_char", "MTIX-3\x00"},
		{"newline", "MTIX-3\n"},
		{"glob_star", "MTIX-3*"},
		{"glob_question", "MTIX-3?"},
		{"tilde", "MTIX-~3"},
		{"pipe", "MTIX-3|x"},
		{"ampersand", "MTIX-3&x"},
		{"backtick", "MTIX-3`x`"},
		{"single_quote", "MTIX-3'x"},
		{"double_quote", "MTIX-3\"x"},
		{"double_dot_segment", "MGIT-1..2"},
		{"leading_dash_body", "MGIT--1"},
		{"trailing_dot", "MGIT-1.2."},
		{"trailing_dash", "MGIT-1-"},
		{"leading_dot", "MGIT-.1"},
		{"no_dash", "MGIT123"},
		{"just_prefix", "MGIT-"},
		{"no_prefix", "-1.2"},
		{"prefix_with_dot", "MG.IT-1"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := ParseTaskID(tt.input)
			assert.Error(t, err, "ParseTaskID(%q) must be rejected", tt.input)
		})
	}
}

// TestTaskID_Parse_ErrorNamesGrammar confirms the rejection error is
// actionable: it quotes the offending id and states the accepted form so a
// user/agent can self-correct (MGIT-41).
func TestTaskID_Parse_ErrorNamesGrammar(t *testing.T) {
	_, err := ParseTaskID("MTIX 30")
	require.Error(t, err)
	msg := err.Error()
	assert.Contains(t, msg, `"MTIX 30"`, "error must quote the offending id")
	assert.Contains(t, msg, "letters", "error must describe the accepted character class")
	assert.Contains(t, msg, "MTIX-30.6", "error must give a concrete example")
}

// TestValidateTaskIDField_ErrorNamesGrammar confirms the shared field
// validator (used by worktree/sandbox/egress option Validate methods) also
// surfaces the actionable grammar message (MGIT-41).
func TestValidateTaskIDField_ErrorNamesGrammar(t *testing.T) {
	tests := []struct {
		name  string
		input string
	}{
		{"empty", ""},
		{"space", "MTIX 30"},
		{"injection", "MTIX-30;rm"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := validateTaskIDField(tt.input)
			require.Error(t, err)
		})
	}

	// Non-empty but malformed must name the grammar.
	err := validateTaskIDField("MTIX 30")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "MTIX-30.6", "field validator must surface the accepted form")

	// Accepted broadened form passes.
	assert.NoError(t, validateTaskIDField("MTIX-30-probe"))
}

func TestTaskID_Parse_AllFormats(t *testing.T) {
	// Verify 1, 2, and 3 part formats all work
	tests := []struct {
		name  string
		input string
		depth int
	}{
		{"depth_1", "MGIT-5", 1},
		{"depth_2", "MGIT-5.3", 2},
		{"depth_3", "MGIT-5.3.1", 3},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			tid, err := ParseTaskID(tt.input)
			require.NoError(t, err)
			assert.Equal(t, tt.depth, tid.Depth())
		})
	}
}

func TestTaskID_ParseInvalid(t *testing.T) {
	tests := []struct {
		name  string
		input string
	}{
		{"empty", ""},
		{"no_prefix", "123"},
		{"no_dash", "MGIT123"},
		{"trailing_dot", "MGIT-1.2."},
		{"leading_dot", "MGIT-.1.2"},
		{"double_dot", "MGIT-1..2"},
		{"spaces", "MGIT- 1.2"},
		{"just_prefix", "MGIT-"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := ParseTaskID(tt.input)
			assert.Error(t, err, "ParseTaskID(%q) should fail", tt.input)
		})
	}
}

func TestTaskID_Validate(t *testing.T) {
	tid, err := ParseTaskID("MGIT-1.2.3")
	require.NoError(t, err)
	assert.NoError(t, tid.Validate(), "valid TaskID should pass validation")

	// Zero-value TaskID should fail validation
	var zero TaskID
	assert.Error(t, zero.Validate(), "zero-value TaskID should fail validation")
}

func TestTaskID_String(t *testing.T) {
	tests := []struct {
		name  string
		input string
		want  string
	}{
		{"canonical", "MGIT-1.2.3", "MGIT-1.2.3"},
		{"single", "PROJ-7", "PROJ-7"},
		{"two_parts", "TEST-3.14", "TEST-3.14"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			tid, err := ParseTaskID(tt.input)
			require.NoError(t, err)
			assert.Equal(t, tt.want, tid.String())
		})
	}
}

func TestTaskID_Comparable(t *testing.T) {
	// TaskID must be usable as map key
	a, err := ParseTaskID("MGIT-1.2.3")
	require.NoError(t, err)
	b, err := ParseTaskID("MGIT-1.2.3")
	require.NoError(t, err)
	c, err := ParseTaskID("MGIT-4.5.6")
	require.NoError(t, err)

	assert.Equal(t, a, b, "same input should produce equal TaskIDs")
	assert.NotEqual(t, a, c, "different input should produce different TaskIDs")

	// Use as map key
	m := map[TaskID]string{a: "first"}
	assert.Equal(t, "first", m[b], "TaskID must work as map key")
}

func TestTaskID_Prefix(t *testing.T) {
	tid, err := ParseTaskID("MGIT-1.2.3")
	require.NoError(t, err)
	assert.Equal(t, "MGIT", tid.Prefix())
}

func TestTaskID_Parent(t *testing.T) {
	tests := []struct {
		name      string
		input     string
		hasParent bool
		parent    string
	}{
		{"depth_3", "MGIT-1.2.3", true, "MGIT-1.2"},
		{"depth_2", "MGIT-1.2", true, "MGIT-1"},
		{"depth_1", "MGIT-1", false, ""},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			tid, err := ParseTaskID(tt.input)
			require.NoError(t, err)
			parent, ok := tid.Parent()
			assert.Equal(t, tt.hasParent, ok)
			if ok {
				assert.Equal(t, tt.parent, parent.String())
			}
		})
	}
}

func TestTaskID_JSONRoundtrip(t *testing.T) {
	original, err := ParseTaskID("MGIT-1.2.3")
	require.NoError(t, err)

	data, err := json.Marshal(original)
	require.NoError(t, err)
	assert.Equal(t, `"MGIT-1.2.3"`, string(data))

	var restored TaskID
	err = json.Unmarshal(data, &restored)
	require.NoError(t, err)
	assert.Equal(t, original, restored)
}
