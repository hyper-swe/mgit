package git

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// fakeEntry is a directory entry whose stat can be made to fail, standing in
// for a path another process removes between the listing and the stat.
type fakeEntry struct {
	name    string
	dir     bool
	infoErr error
}

func (e fakeEntry) Name() string { return e.name }
func (e fakeEntry) IsDir() bool  { return e.dir }
func (e fakeEntry) Type() fs.FileMode {
	if e.dir {
		return fs.ModeDir
	}
	return 0
}

func (e fakeEntry) Info() (fs.FileInfo, error) {
	if e.infoErr != nil {
		return nil, e.infoErr
	}
	return fakeInfo{name: e.name, dir: e.dir}, nil
}

type fakeInfo struct {
	name string
	dir  bool
}

func (i fakeInfo) Name() string { return i.name }
func (i fakeInfo) Size() int64  { return 0 }
func (i fakeInfo) Mode() fs.FileMode {
	if i.dir {
		return fs.ModeDir
	}
	return 0
}
func (i fakeInfo) ModTime() time.Time { return time.Time{} }
func (i fakeInfo) IsDir() bool        { return i.dir }
func (i fakeInfo) Sys() any           { return nil }

func namesOf(infos []os.FileInfo) []string {
	out := make([]string, 0, len(infos))
	for _, i := range infos {
		out = append(out, i.Name())
	}
	return out
}

func fsWith(root string, entries []os.DirEntry, err error) *ignoreReadFS {
	f := newIgnoreReadFS(root)
	f.readDir = func(string) ([]os.DirEntry, error) { return entries, err }
	return f
}

// Table of what a listing shows. Each case is a property of the reader the
// ignore rules are read through, not of any one scenario. Refs: MGIT-285
func TestIgnoreReadFS_ReadDir_ListsOnlyWhatTheProjectWalkEnters(t *testing.T) {
	root := t.TempDir()
	require.NoError(t, os.MkdirAll(filepath.Join(root, "wt-1", ".mgit"), 0o750))
	require.NoError(t, os.MkdirAll(filepath.Join(root, "pkg"), 0o750))
	gone := fs.ErrNotExist
	tests := []struct {
		name    string
		entries []os.DirEntry
		want    []string
	}{
		{"plain_entries_are_listed", []os.DirEntry{fakeEntry{name: "a.go"}, fakeEntry{name: "pkg", dir: true}}, []string{"a.go", "pkg"}},
		{"store_and_git_dirs_are_absent", []os.DirEntry{fakeEntry{name: ".mgit", dir: true}, fakeEntry{name: ".git", dir: true}, fakeEntry{name: "a.go"}}, []string{"a.go"}},
		{"nested_mgit_root_is_absent", []os.DirEntry{fakeEntry{name: "wt-1", dir: true}, fakeEntry{name: "pkg", dir: true}}, []string{"pkg"}},
		{"name_that_vanished_is_absent", []os.DirEntry{fakeEntry{name: "gone.tmp", infoErr: gone}, fakeEntry{name: "a.go"}}, []string{"a.go"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			infos, err := fsWith(root, tt.entries, nil).ReadDir(".")

			require.NoError(t, err)
			assert.Equal(t, tt.want, namesOf(infos))
		})
	}
}

// Only a name that is GONE is skipped; any other failure to stat an entry, or
// to list the directory at all, is reported, so a real fault is never read as
// "nothing there". Refs: MGIT-285
func TestIgnoreReadFS_ReadDir_ReportsEveryFailureThatIsNotAVanishedName(t *testing.T) {
	boom := errors.New("input/output error")
	tests := []struct {
		name    string
		entries []os.DirEntry
		listErr error
	}{
		{"stat_failure_on_an_entry", []os.DirEntry{fakeEntry{name: "a.go", infoErr: boom}}, nil},
		{"listing_failure", nil, boom},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := fsWith(t.TempDir(), tt.entries, tt.listErr).ReadDir(".")

			require.ErrorIs(t, err, boom)
		})
	}
}

// A directory that vanishes between its parent's listing and its own (a
// worktree directory rolled back mid-creation, before its store exists) is
// gone, not a fault: it holds no rules to read. The project root itself
// vanishing is a fault and is reported. Refs: MGIT-285
func TestIgnoreReadFS_ReadDir_ASubdirectoryThatVanishedIsEmptyButTheRootIsAFault(t *testing.T) {
	gone := &os.PathError{Op: "open", Path: "x", Err: fs.ErrNotExist}
	tests := []struct {
		name    string
		path    string
		wantErr bool
	}{
		{"vanished_subdirectory_is_empty", "wt-rolled-back", false},
		{"vanished_nested_subdirectory_is_empty", "a/b", false},
		{"vanished_project_root_is_a_fault", ".", true},
		{"vanished_project_root_by_empty_path_is_a_fault", "", true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			infos, err := fsWith(t.TempDir(), nil, gone).ReadDir(tt.path)

			if tt.wantErr {
				require.ErrorIs(t, err, fs.ErrNotExist)
				return
			}
			require.NoError(t, err)
			assert.Empty(t, infos)
		})
	}
}
