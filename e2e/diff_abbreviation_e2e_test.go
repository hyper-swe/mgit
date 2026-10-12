package e2e

import (
	"fmt"
	"path/filepath"
	"strings"
	"testing"

	gogit "github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/object"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Real CLI diffs with unique abbreviations equal full-ID diffs. Unknown and
// ambiguous operands must fail clearly, never select a candidate. Refs: MGIT-292
func TestE2E_Diff_Abbreviations(t *testing.T) {
	bin := buildMgitBinary(t)
	root := t.TempDir()
	mustMgit(t, bin, root, "init")
	gr, err := gogit.PlainOpen(filepath.Join(root, ".mgit"))
	require.NoError(t, err)
	first, err := gr.Head()
	require.NoError(t, err)
	writeIgnoredFixture(t, root, "payload.txt", "added content\n")
	mustMgit(t, bin, root, "add", "payload.txt")
	mustMgit(t, bin, root, "commit", "--task-id", "MGIT-292.CLI", "-m", "payload")
	last, err := gr.Head()
	require.NoError(t, err)
	from, to := first.Hash().String(), last.Hash().String()
	full := mustMgit(t, bin, root, "diff", "--from", from, "--to", to)
	require.Contains(t, full, "added content")
	for i, operands := range [][2]string{{from[:12], to}, {from, to[:12]}, {strings.ToUpper(from[:12]), to[:12]}} {
		t.Run(fmt.Sprintf("unique_%d", i), func(t *testing.T) {
			got, err := runMgit(t, bin, root, "diff", "--from", operands[0], "--to", operands[1])
			require.NoError(t, err, got)
			assert.Equal(t, full, got)
		})
	}
	for _, ref := range []string{strings.Repeat("0", 40), strings.Repeat("0", 12), "abc", "not-hex"} {
		for i, operands := range [][2]string{{ref, to}, {from, ref}} {
			t.Run(fmt.Sprintf("unknown_%s_%d", ref, i), func(t *testing.T) {
				out, err := runMgit(t, bin, root, "diff", "--from", operands[0], "--to", operands[1])
				require.Error(t, err)
				assert.Contains(t, out, "commit not found")
			})
		}
	}
	prefix := plantCLIDiffCollision(t, gr)
	for i, operands := range [][2]string{{prefix, to}, {from, prefix}} {
		t.Run(fmt.Sprintf("ambiguous_%d", i), func(t *testing.T) {
			out, err := runMgit(t, bin, root, "diff", "--from", operands[0], "--to", operands[1])
			require.Error(t, err)
			assert.Contains(t, out, "ambiguous commit hash prefix")
		})
	}
}

func plantCLIDiffCollision(t *testing.T, repo *gogit.Repository) string {
	t.Helper()
	head, err := repo.Head()
	require.NoError(t, err)
	parent, err := repo.CommitObject(head.Hash())
	require.NoError(t, err)
	seen := make(map[string]*plumbing.MemoryObject)
	for i := 0; i <= 65536; i++ {
		c := &object.Commit{TreeHash: parent.TreeHash, Author: parent.Author, Committer: parent.Committer, Message: fmt.Sprintf("collision %d", i)}
		obj := &plumbing.MemoryObject{}
		require.NoError(t, c.Encode(obj))
		prefix := obj.Hash().String()[:4]
		if prior, ok := seen[prefix]; ok {
			_, err = repo.Storer.SetEncodedObject(prior)
			require.NoError(t, err)
			_, err = repo.Storer.SetEncodedObject(obj)
			require.NoError(t, err)
			return prefix
		}
		seen[prefix] = obj
	}
	t.Fatal("no collision within the four-digit pigeonhole bound")
	return ""
}
