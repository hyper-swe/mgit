package main

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// projectPath resolves a typed path against the working directory, then
// against the project root, as git does. Refs: MGIT-278.1
func TestProjectPath_ResolvesLikeGit(t *testing.T) {
	root := t.TempDir()
	require.NoError(t, os.MkdirAll(filepath.Join(root, "pkg", "sub"), 0o750))
	pkg := filepath.Join(root, "pkg")
	tests := []struct {
		name, cwd, arg, want string
	}{
		{"dot_in_a_subdirectory_is_the_subdirectory", pkg, ".", "pkg"},
		{"dot_at_the_root_is_the_root", root, ".", "."},
		{"a_relative_file", pkg, "a.go", "pkg/a.go"},
		{"a_relative_directory", pkg, "sub", "pkg/sub"},
		{"stepping_out_of_the_subdirectory", pkg, "../other/o.go", "other/o.go"},
		{"a_deleted_file", pkg, "gone.go", "pkg/gone.go"},
		{"an_absolute_path_inside", pkg, filepath.Join(root, "x.go"), "x.go"},
		{"root_relative_from_the_root", root, "pkg/a.go", "pkg/a.go"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := projectPath(root, tt.cwd, tt.arg)
			require.NoError(t, err)
			assert.Equal(t, tt.want, got)
		})
	}
}

func TestProjectPath_OutsideTheProject_Refused(t *testing.T) {
	root := t.TempDir()
	for _, arg := range []string{"../outside.go", "..", filepath.Join(filepath.Dir(root), "elsewhere")} {
		_, err := projectPath(root, root, arg)
		assert.Error(t, err, arg)
	}
}

// The working directory and the root can be one directory under two
// spellings, such as a symlink to it (macOS's /var is /private/var).
func TestProjectPath_RootReachedThroughASymlink_IsTheSameRoot(t *testing.T) {
	real := t.TempDir()
	require.NoError(t, os.MkdirAll(filepath.Join(real, "pkg"), 0o750))
	link := filepath.Join(t.TempDir(), "link")
	require.NoError(t, os.Symlink(real, link))

	got, err := projectPath(real, filepath.Join(link, "pkg"), "a.go")
	require.NoError(t, err)
	assert.Equal(t, "pkg/a.go", got)
}
