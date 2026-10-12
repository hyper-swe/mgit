package main

import (
	"errors"
	"io/fs"
	"os"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// fakeDir is a directory in a fake stat table.
type fakeDir struct{ name string }

func (d fakeDir) Name() string     { return d.name }
func (fakeDir) Size() int64        { return 0 }
func (fakeDir) Mode() fs.FileMode  { return fs.ModeDir | 0o755 }
func (fakeDir) ModTime() time.Time { return time.Time{} }
func (fakeDir) IsDir() bool        { return true }
func (fakeDir) Sys() any           { return nil }

// statIn answers os.Stat from a set of existing directories.
func statIn(dirs ...string) func(string) (os.FileInfo, error) {
	set := map[string]bool{}
	for _, d := range dirs {
		set[d] = true
	}
	return func(p string) (os.FileInfo, error) {
		if set[p] {
			return fakeDir{name: p}, nil
		}
		return nil, fs.ErrNotExist
	}
}

func TestDeepestExistingAncestor(t *testing.T) {
	stat := statIn("/", "/home", "/var", "/var/tmp")
	tests := []struct {
		path string
		want string
	}{
		{"/home/alice/src/wt", "/home"},
		{"/var/tmp/mgit-e2e-1/wt", "/var/tmp"},
		{"/var/lib/x/wt", "/var"},
		{"/work/wt", "/"},
	}
	for _, tt := range tests {
		t.Run(tt.path, func(t *testing.T) {
			assert.Equal(t, tt.want, deepestExistingAncestor(tt.path, stat))
		})
	}
}

var errCopyUp = errors.New("operation not supported")

func isCopyUp(err error) bool { return errors.Is(err, errCopyUp) }

// The mount point is created normally wherever that works; the shadow is a
// repair applied ONLY where the guest root refused the copy-up, and only to
// the deepest directory that already exists, so the least of the image is
// moved into memory. Refs: MGIT-230.7, MGIT-89
func TestMakeMountPoint(t *testing.T) {
	tests := []struct {
		name        string
		mkdirErrs   []error // one per MkdirAll call, in order
		shadowErr   error
		wantShadow  []string
		wantErr     string
		wantMkdirs  int
		existingDir []string
	}{
		{name: "creatable_first_time_no_shadow", mkdirErrs: []error{nil}, wantMkdirs: 1},
		{
			name:       "copy_up_refused_shadows_the_deepest_existing_ancestor_then_creates",
			mkdirErrs:  []error{errCopyUp, nil},
			wantShadow: []string{"/home"}, wantMkdirs: 2,
		},
		{
			name:      "another_error_is_returned_without_a_shadow",
			mkdirErrs: []error{fs.ErrPermission}, wantMkdirs: 1,
			wantErr: "permission denied",
		},
		{
			name:      "a_failed_shadow_is_named",
			mkdirErrs: []error{errCopyUp}, shadowErr: errors.New("mount tmpfs over /home: EPERM"),
			wantShadow: []string{"/home"}, wantMkdirs: 1,
			wantErr: "/home",
		},
		{
			name:       "still_refused_after_the_shadow_is_an_error",
			mkdirErrs:  []error{errCopyUp, errCopyUp},
			wantShadow: []string{"/home"}, wantMkdirs: 2,
			wantErr: "still",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var mkdirs int
			var shadowed []string
			err := makeMountPoint("/home/alice/src/wt", mountPointOps{
				mkdirAll: func(string, os.FileMode) error {
					require.Less(t, mkdirs, len(tt.mkdirErrs), "unexpected extra MkdirAll")
					e := tt.mkdirErrs[mkdirs]
					mkdirs++
					return e
				},
				stat:            statIn("/", "/home"),
				isCopyUpRefusal: isCopyUp,
				shadow: func(dir string) error {
					shadowed = append(shadowed, dir)
					return tt.shadowErr
				},
			})
			assert.Equal(t, tt.wantMkdirs, mkdirs)
			assert.Equal(t, tt.wantShadow, shadowed)
			if tt.wantErr == "" {
				assert.NoError(t, err)
			} else {
				require.Error(t, err)
				assert.Contains(t, err.Error(), tt.wantErr)
			}
		})
	}
}

// The root itself is never shadowed: a tmpfs over / would hide the whole
// image. A mount point directly under / is created in the overlay's upper
// with no copy-up, so reaching here means something else is wrong — said so.
func TestMakeMountPoint_NeverShadowsTheRoot(t *testing.T) {
	var shadowed []string
	err := makeMountPoint("/work/wt", mountPointOps{
		mkdirAll:        func(string, os.FileMode) error { return errCopyUp },
		stat:            statIn("/"),
		isCopyUpRefusal: isCopyUp,
		shadow:          func(dir string) error { shadowed = append(shadowed, dir); return nil },
	})
	require.Error(t, err)
	assert.Empty(t, shadowed)
	assert.Contains(t, err.Error(), "root")
}
