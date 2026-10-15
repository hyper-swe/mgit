package service

import (
	"reflect"
	"testing"

	"github.com/stretchr/testify/assert"
)

// SquashService had a patch renderer that built text from model.FileDiff
// metadata. It cannot emit a correct patch (wrong /dev/null header for an added
// file, hunk line numbers that always start at 1, and an empty body on a
// dry-run squash, which is how MGIT-112 shipped a patch that applied to
// nothing), and it has no production caller. Every patch that leaves mgit goes
// through GitFormatPatch or PreviewGitPatch. This keeps the old renderer from
// coming back under its old name. Refs: MGIT-114, MGIT-112
func TestSquashService_HasNoMetadataPatchRenderer(t *testing.T) {
	_, found := reflect.TypeOf(&SquashService{}).MethodByName("ExportToGitPatch")
	assert.False(t, found, "SquashService.ExportToGitPatch must stay deleted: use GitFormatPatch or PreviewGitPatch")
	for _, name := range []string{"GitFormatPatch", "PreviewGitPatch"} {
		_, ok := reflect.TypeOf(&SquashService{}).MethodByName(name)
		assert.True(t, ok, "%s is the real renderer and must exist", name)
	}
}
