package model

import (
	"encoding/json"
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
		{"lowercase_prefix", "mgit-1.2.3"},
		{"trailing_dot", "MGIT-1.2."},
		{"leading_dot", "MGIT-.1.2"},
		{"double_dot", "MGIT-1..2"},
		{"non_numeric", "MGIT-abc"},
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
