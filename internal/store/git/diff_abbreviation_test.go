package git

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/object"
	"github.com/hyper-swe/mgit/internal/model"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Diff and patch use the same unique reference semantics as commit lookup.
// Refs: MGIT-292, FR-11
func TestDiffStore_Abbreviations(t *testing.T) {
	repo := initTestRepo(t)
	ctx := context.Background()
	from, err := repo.Head()
	require.NoError(t, err)
	writeFileMk(t, repo.Root(), "payload.txt", "added content\n")
	require.NoError(t, NewWorktreeStore(repo).Add(ctx, "payload.txt"))
	to, err := NewCommitStore(repo).CreateCommit(ctx, makeTestModelCommit(t, "MGIT-292.TEST"))
	require.NoError(t, err)
	ds := NewDiffStore(repo)
	full, err := ds.DiffCommits(ctx, from, to)
	require.NoError(t, err)
	require.Len(t, full, 1)
	patch, err := ds.PatchBetween(ctx, from, to)
	require.NoError(t, err)
	for i, operands := range [][2]string{{from[:12], to}, {from, to[:12]}, {strings.ToUpper(from[:12]), to[:12]}} {
		t.Run(fmt.Sprintf("unique_%d", i), func(t *testing.T) {
			abbreviated, err := ds.DiffCommits(ctx, operands[0], operands[1])
			require.NoError(t, err)
			assert.Equal(t, full, abbreviated)
			got, err := ds.PatchBetween(ctx, operands[0], operands[1])
			require.NoError(t, err)
			assert.Equal(t, patch, got)
		})
	}
	for _, ref := range []string{strings.Repeat("0", 40), strings.Repeat("0", 12), "abc", "not-hex"} {
		for i, operands := range [][2]string{{ref, to}, {from, ref}} {
			t.Run(fmt.Sprintf("unknown_%s_%d", ref, i), func(t *testing.T) {
				_, err := ds.DiffCommits(ctx, operands[0], operands[1])
				require.ErrorIs(t, err, model.ErrCommitNotFound)
				_, err = ds.PatchBetween(ctx, operands[0], operands[1])
				require.ErrorIs(t, err, model.ErrCommitNotFound)
			})
		}
	}
	prefix := plantDiffCollision(t, repo)
	for i, operands := range [][2]string{{prefix, to}, {from, prefix}} {
		t.Run(fmt.Sprintf("ambiguous_%d", i), func(t *testing.T) {
			_, err := ds.DiffCommits(ctx, operands[0], operands[1])
			require.ErrorIs(t, err, model.ErrAmbiguousHash)
			_, err = ds.PatchBetween(ctx, operands[0], operands[1])
			require.ErrorIs(t, err, model.ErrAmbiguousHash)
		})
	}
}

// The pigeonhole bound guarantees two independently encoded commit objects
// share four hex digits. Store only that pair; refs and user Git stay untouched.
func plantDiffCollision(t *testing.T, repo *Repository) string {
	t.Helper()
	head, err := repo.repo.Head()
	require.NoError(t, err)
	parent, err := repo.repo.CommitObject(head.Hash())
	require.NoError(t, err)
	seen := make(map[string]*plumbing.MemoryObject)
	for i := 0; i <= 65536; i++ {
		c := &object.Commit{TreeHash: parent.TreeHash, Author: parent.Author, Committer: parent.Committer, Message: fmt.Sprintf("collision %d", i)}
		obj := &plumbing.MemoryObject{}
		require.NoError(t, c.Encode(obj))
		prefix := obj.Hash().String()[:4]
		if prior, ok := seen[prefix]; ok {
			_, err = repo.repo.Storer.SetEncodedObject(prior)
			require.NoError(t, err)
			_, err = repo.repo.Storer.SetEncodedObject(obj)
			require.NoError(t, err)
			return prefix
		}
		seen[prefix] = obj
	}
	t.Fatal("no collision within the four-digit pigeonhole bound")
	return ""
}
