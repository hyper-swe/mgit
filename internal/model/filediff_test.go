package model

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestFileDiff_AllFieldsPresent(t *testing.T) {
	fd := FileDiff{
		Path:       "internal/model/commit.go",
		Operation:  DiffAdded,
		OldHash:    "",
		NewHash:    "abc123",
		Hunks:      []Hunk{{LineStart: 1, LinesAdded: 10, LinesRemoved: 0, Content: "+new code"}},
		BinaryDiff: false,
	}
	assert.Equal(t, "internal/model/commit.go", fd.Path)
	assert.Equal(t, DiffAdded, fd.Operation)
	assert.Equal(t, "", fd.OldHash)
	assert.Equal(t, "abc123", fd.NewHash)
	assert.Len(t, fd.Hunks, 1)
	assert.False(t, fd.BinaryDiff)
}

func TestFileDiff_Validate(t *testing.T) {
	tests := []struct {
		name    string
		diff    FileDiff
		wantErr bool
	}{
		{
			name:    "valid_added",
			diff:    FileDiff{Path: "foo.go", Operation: DiffAdded, NewHash: "abc"},
			wantErr: false,
		},
		{
			name:    "valid_modified",
			diff:    FileDiff{Path: "foo.go", Operation: DiffModified, OldHash: "old", NewHash: "new"},
			wantErr: false,
		},
		{
			name:    "empty_path",
			diff:    FileDiff{Path: "", Operation: DiffAdded},
			wantErr: true,
		},
		{
			name:    "invalid_operation",
			diff:    FileDiff{Path: "foo.go", Operation: "invalid"},
			wantErr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := tt.diff.Validate()
			if tt.wantErr {
				assert.Error(t, err)
			} else {
				assert.NoError(t, err)
			}
		})
	}
}

func TestFileDiff_Statistics(t *testing.T) {
	fd := FileDiff{
		Path:      "main.go",
		Operation: DiffModified,
		Hunks: []Hunk{
			{LineStart: 1, LinesAdded: 5, LinesRemoved: 3, Content: "hunk1"},
			{LineStart: 20, LinesAdded: 10, LinesRemoved: 2, Content: "hunk2"},
		},
	}

	stats := fd.Statistics()
	assert.Equal(t, 15, stats.LinesAdded)
	assert.Equal(t, 5, stats.LinesRemoved)
}

func TestFileDiff_Statistics_Empty(t *testing.T) {
	fd := FileDiff{
		Path:      "empty.go",
		Operation: DiffAdded,
	}
	stats := fd.Statistics()
	assert.Equal(t, 0, stats.LinesAdded)
	assert.Equal(t, 0, stats.LinesRemoved)
}

func TestFileDiff_BinaryFlag(t *testing.T) {
	fd := FileDiff{
		Path:       "image.png",
		Operation:  DiffAdded,
		NewHash:    "abc",
		BinaryDiff: true,
	}
	assert.True(t, fd.BinaryDiff)
	assert.NoError(t, fd.Validate())

	stats := fd.Statistics()
	assert.Equal(t, 0, stats.LinesAdded, "binary files should have zero line stats")
}

func TestFileDiff_JSONRoundtrip(t *testing.T) {
	original := FileDiff{
		Path:      "main.go",
		Operation: DiffModified,
		OldHash:   "aaa",
		NewHash:   "bbb",
		Hunks: []Hunk{
			{LineStart: 1, LinesAdded: 3, LinesRemoved: 1, Content: "changes"},
		},
		BinaryDiff: false,
	}

	data, err := json.Marshal(original)
	require.NoError(t, err)

	var restored FileDiff
	err = json.Unmarshal(data, &restored)
	require.NoError(t, err)

	assert.Equal(t, original, restored)
}

func TestFileDiff_Operations(t *testing.T) {
	ops := []DiffOperation{DiffAdded, DiffModified, DiffDeleted, DiffRenamed}
	for _, op := range ops {
		t.Run(string(op), func(t *testing.T) {
			fd := FileDiff{Path: "test.go", Operation: op, NewHash: "x"}
			assert.NoError(t, fd.Validate())
		})
	}
}
