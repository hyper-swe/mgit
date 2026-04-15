package service

import (
	"testing"

	"github.com/stretchr/testify/assert"

	"github.com/hyper-swe/mgit/internal/model"
)

func TestDiffService_FormatUnified_Added(t *testing.T) {
	ds := &DiffService{}
	diffs := []model.FileDiff{
		{Path: "main.go", Operation: model.DiffAdded, NewHash: "abc12345def"},
	}
	out := ds.FormatUnified(diffs)
	assert.Contains(t, out, "diff --mgit a/main.go b/main.go")
	assert.Contains(t, out, "new file: main.go")
	assert.Contains(t, out, "+++ b/main.go (abc12345)")
}

func TestDiffService_FormatUnified_Modified(t *testing.T) {
	ds := &DiffService{}
	diffs := []model.FileDiff{
		{
			Path: "main.go", Operation: model.DiffModified,
			OldHash: "11112222", NewHash: "33334444",
			Hunks: []model.Hunk{
				{LineStart: 10, LinesAdded: 3, LinesRemoved: 1, Content: "+new line\n-old line\n"},
			},
		},
	}
	out := ds.FormatUnified(diffs)
	assert.Contains(t, out, "modified: main.go")
	assert.Contains(t, out, "@@ -10,1 +10,3 @@")
	assert.Contains(t, out, "+new line")
	assert.Contains(t, out, "-old line")
}

func TestDiffService_FormatUnified_Deleted(t *testing.T) {
	ds := &DiffService{}
	diffs := []model.FileDiff{
		{Path: "old.go", Operation: model.DiffDeleted, OldHash: "deadbeef"},
	}
	out := ds.FormatUnified(diffs)
	assert.Contains(t, out, "deleted file: old.go")
}

func TestDiffService_FormatStat(t *testing.T) {
	ds := &DiffService{}
	diffs := []model.FileDiff{
		{
			Path: "a.go", Operation: model.DiffModified,
			Hunks: []model.Hunk{{LinesAdded: 5, LinesRemoved: 2}},
		},
		{
			Path: "b.go", Operation: model.DiffAdded,
			Hunks: []model.Hunk{{LinesAdded: 10, LinesRemoved: 0}},
		},
	}
	out := ds.FormatStat(diffs)
	assert.Contains(t, out, "a.go")
	assert.Contains(t, out, "b.go")
	assert.Contains(t, out, "2 files changed, 15 insertions(+), 2 deletions(-)")
}

func TestDiffService_FormatUnified_Empty(t *testing.T) {
	ds := &DiffService{}
	out := ds.FormatUnified(nil)
	assert.Empty(t, out)
}

func TestDiffService_FormatStat_Empty(t *testing.T) {
	ds := &DiffService{}
	out := ds.FormatStat(nil)
	assert.Contains(t, out, "0 files changed, 0 insertions(+), 0 deletions(-)")
}
