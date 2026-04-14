package git

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/hyper-swe/mgit-dev/internal/model"
)

func TestCommitStore_GetFileFromCommit_ValidFile(t *testing.T) {
	repo := initTestRepo(t)
	ctx := context.Background()
	ws := NewWorktreeStore(repo)
	cs := NewCommitStore(repo)

	// Write a file, stage, and commit
	fileContent := "package hello\n\nfunc Hello() string { return \"world\" }\n"
	err := os.WriteFile(filepath.Join(repo.Root(), "hello.go"), []byte(fileContent), 0o600)
	require.NoError(t, err)
	require.NoError(t, ws.Add(ctx, "hello.go"))

	c := makeTestModelCommit(t, "MGIT-1.1")
	hash, err := cs.CreateCommit(ctx, c)
	require.NoError(t, err)

	// Retrieve the file from the commit
	data, err := cs.GetFileFromCommit(ctx, hash, "hello.go")
	require.NoError(t, err)
	assert.Equal(t, fileContent, string(data))
}

func TestCommitStore_GetFileFromCommit_MissingFile(t *testing.T) {
	repo := initTestRepo(t)
	ctx := context.Background()
	cs := NewCommitStore(repo)

	head, err := repo.Head()
	require.NoError(t, err)

	_, err = cs.GetFileFromCommit(ctx, head, "nonexistent.go")
	assert.Error(t, err)
	assert.ErrorIs(t, err, model.ErrFileNotFound)
}

func TestCommitStore_GetFileFromCommit_MissingCommit(t *testing.T) {
	repo := initTestRepo(t)
	ctx := context.Background()
	cs := NewCommitStore(repo)

	_, err := cs.GetFileFromCommit(ctx, "0000000000000000000000000000000000000000", "any.go")
	assert.Error(t, err)
	assert.ErrorIs(t, err, model.ErrCommitNotFound)
}

func TestCommitStore_GetFileFromCommit_EmptyPath(t *testing.T) {
	repo := initTestRepo(t)
	ctx := context.Background()
	cs := NewCommitStore(repo)

	head, err := repo.Head()
	require.NoError(t, err)

	_, err = cs.GetFileFromCommit(ctx, head, "")
	assert.Error(t, err, "empty path must be rejected")
}

func TestCommitStore_GetFileFromCommit_TableDriven(t *testing.T) {
	repo := initTestRepo(t)
	ctx := context.Background()
	ws := NewWorktreeStore(repo)
	cs := NewCommitStore(repo)

	// Create a commit with a known file
	err := os.WriteFile(filepath.Join(repo.Root(), "data.txt"), []byte("hello\n"), 0o600)
	require.NoError(t, err)
	require.NoError(t, ws.Add(ctx, "data.txt"))

	c := makeTestModelCommit(t, "MGIT-2.1")
	commitHash, err := cs.CreateCommit(ctx, c)
	require.NoError(t, err)

	tests := []struct {
		name    string
		hash    string
		path    string
		wantErr error
	}{
		{
			name:    "valid_file",
			hash:    commitHash,
			path:    "data.txt",
			wantErr: nil,
		},
		{
			name:    "missing_file_in_valid_commit",
			hash:    commitHash,
			path:    "missing.txt",
			wantErr: model.ErrFileNotFound,
		},
		{
			name:    "invalid_commit_hash",
			hash:    "0000000000000000000000000000000000000000",
			path:    "data.txt",
			wantErr: model.ErrCommitNotFound,
		},
		{
			name:    "empty_path",
			hash:    commitHash,
			path:    "",
			wantErr: nil, // non-nil error but not a sentinel
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			data, err := cs.GetFileFromCommit(ctx, tt.hash, tt.path)
			if tt.wantErr != nil {
				assert.ErrorIs(t, err, tt.wantErr)
				assert.Nil(t, data)
			} else if tt.path == "" {
				assert.Error(t, err, "empty path must error")
			} else {
				require.NoError(t, err)
				assert.NotEmpty(t, data)
			}
		})
	}
}
