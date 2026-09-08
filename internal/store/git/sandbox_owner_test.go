package git

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// A directory that `mgit sandbox launch --worktree` decorates is neither a
// repository nor a linked worktree, and after the decoration it carried a
// .mgit of its own with nothing in it that names who registered its sandbox —
// so every verb run from inside it addressed a phantom repository (MGIT-196,
// FEAT-7.28). The owner file is that name. Refs: MGIT-196
func TestSandboxOwner_WriteReadAbsentAndCorrupt(t *testing.T) {
	tests := []struct {
		name    string
		prepare func(t *testing.T, dir string)
		want    *SandboxOwner
		wantOK  bool
		wantErr string
	}{
		{name: "absent_is_not_an_error", prepare: func(*testing.T, string) {}},
		{
			name: "written_then_read",
			prepare: func(t *testing.T, dir string) {
				require.NoError(t, WriteSandboxOwner(dir, SandboxOwner{RepoRoot: "/repo", Task: "T-1"}))
			},
			want: &SandboxOwner{RepoRoot: "/repo", Task: "T-1"}, wantOK: true,
		},
		{
			name: "corrupt_is_an_error_not_absence",
			prepare: func(t *testing.T, dir string) {
				require.NoError(t, os.MkdirAll(filepath.Join(dir, ".mgit"), 0o750))
				require.NoError(t, os.WriteFile(filepath.Join(dir, ".mgit", "sandbox-owner"), []byte("{not json"), 0o600))
			},
			wantErr: "parse sandbox owner",
		},
		{
			name: "a_root_less_record_is_invalid",
			prepare: func(t *testing.T, dir string) {
				require.NoError(t, os.MkdirAll(filepath.Join(dir, ".mgit"), 0o750))
				require.NoError(t, os.WriteFile(filepath.Join(dir, ".mgit", "sandbox-owner"), []byte(`{"task":"T-1"}`), 0o600))
			},
			wantErr: "missing repo_root",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dir := t.TempDir()
			tt.prepare(t, dir)
			got, ok, err := ReadSandboxOwner(dir)
			if tt.wantErr != "" {
				require.ErrorContains(t, err, tt.wantErr)
				return
			}
			require.NoError(t, err)
			assert.Equal(t, tt.wantOK, ok)
			assert.Equal(t, tt.want, got)
		})
	}
}

// StorePresent is what tells a repository's .mgit (a store) from the .mgit a
// launch or a worktree leaves behind (markers, shims, an owner file). The
// shapes are laid out by hand so the cases do not derive from the code under
// test. Refs: MGIT-196
func TestStorePresent_TellsAStoreFromDecoration(t *testing.T) {
	base := t.TempDir()
	repo := filepath.Join(base, "repo")
	require.NoError(t, os.MkdirAll(repo, 0o750))
	_, err := Init(repo, func() time.Time { return time.Unix(0, 0).UTC() })
	require.NoError(t, err)

	linked := filepath.Join(base, "linked")
	require.NoError(t, os.MkdirAll(filepath.Join(linked, ".mgit"), 0o750))
	require.NoError(t, os.WriteFile(filepath.Join(linked, ".mgit", "worktree"), []byte(`{"store":"x","branch":"b"}`), 0o600))

	decorated := filepath.Join(base, "decorated")
	require.NoError(t, os.MkdirAll(filepath.Join(decorated, ".mgit", "shims"), 0o750))

	tests := []struct {
		name string
		root string
		want bool
	}{
		{name: "an_initialized_repository", root: repo, want: true},
		{name: "a_linked_worktree_marker_only", root: linked, want: false},
		{name: "launch_decoration_only", root: decorated, want: false},
		{name: "no_mgit_at_all", root: filepath.Join(base, "plain"), want: false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, StorePresent(tt.root))
		})
	}
}

// LaunchDecorated is the evidence the store-less refusal cites: the files a
// launch writes. A bare .mgit is not decoration. Refs: MGIT-196
func TestLaunchDecorated_SeesWhatALaunchWrites(t *testing.T) {
	tests := []struct {
		name  string
		files []string
		want  bool
	}{
		{name: "bare_mgit", files: nil, want: false},
		{name: "generated_list", files: []string{"generated"}, want: true},
		{name: "shims_dir", files: []string{"shims/"}, want: true},
		{name: "only_an_index", files: []string{"sandbox/sandbox-index.db"}, want: false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			root := t.TempDir()
			require.NoError(t, os.MkdirAll(filepath.Join(root, ".mgit"), 0o750))
			for _, f := range tt.files {
				p := filepath.Join(root, ".mgit", filepath.FromSlash(f))
				if f[len(f)-1] == '/' {
					require.NoError(t, os.MkdirAll(p, 0o750))
					continue
				}
				require.NoError(t, os.MkdirAll(filepath.Dir(p), 0o750))
				require.NoError(t, os.WriteFile(p, []byte("x"), 0o600))
			}
			assert.Equal(t, tt.want, LaunchDecorated(root))
		})
	}
}

// ClearSandboxOwner removes only the record naming the task being retired.
// Refs: MGIT-196
func TestClearSandboxOwner_RemovesOnlyThatTasksRecord(t *testing.T) {
	dir := t.TempDir()
	require.NoError(t, ClearSandboxOwner(dir, "T-1"), "no record is not an error")
	require.NoError(t, WriteSandboxOwner(dir, SandboxOwner{RepoRoot: "/repo", Task: "T-1"}))
	require.NoError(t, ClearSandboxOwner(dir, "T-9"))
	_, ok, err := ReadSandboxOwner(dir)
	require.NoError(t, err)
	assert.True(t, ok, "another task's record stays")
	require.NoError(t, ClearSandboxOwner(dir, "T-1"))
	_, ok, err = ReadSandboxOwner(dir)
	require.NoError(t, err)
	assert.False(t, ok, "the retired task's record goes")
}
