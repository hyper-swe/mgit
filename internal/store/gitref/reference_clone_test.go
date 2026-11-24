package gitref

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/go-git/go-git/v5/plumbing"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// referenceClone makes a clone with `git clone --reference`, the shape a
// fleet run produces: the HEAD commit lives only in the reference's object
// store, and the clone names that store in objects/info/alternates.
func referenceClone(t *testing.T, source, reference string) string {
	t.Helper()
	clone := filepath.Join(t.TempDir(), "clone")
	// A file:// URL forces the transport path: a plain path would hardlink
	// the objects into the clone and the reference would not be needed.
	alternatesGit(t, source, "clone", "--reference", reference, "file://"+source, clone)
	return clone
}

// A clone made with `git clone --reference` reads its HEAD tree from the
// reference store; no object may be reported missing. Refs: HSWE-47.6
func TestCommittedContent_ReferenceClone(t *testing.T) {
	source := gitRepoWithCommit(t, "file.txt", "referenced\n")
	reference := filepath.Join(t.TempDir(), "reference")
	alternatesGit(t, source, "clone", "--bare", source, reference)
	clone := referenceClone(t, source, reference)
	_, err := os.Stat(filepath.Join(clone, ".git", "objects", "info", "alternates"))
	require.NoError(t, err, "the clone must borrow from the reference")
	head := strings.TrimSpace(alternatesGit(t, clone, "rev-parse", "HEAD"))
	require.Error(t, statLoose(clone, head), "the HEAD commit must live only in the reference store")

	blobs, err := CommittedBlobs(clone)
	require.NoError(t, err)
	assert.Equal(t, plumbing.ComputeHash(plumbing.BlobObject, []byte("referenced\n")).String(), blobs["file.txt"])
	files, got, err := CommittedFiles(clone)
	require.NoError(t, err)
	require.Len(t, files, 1)
	assert.Equal(t, head, got)
	assert.Equal(t, "referenced\n", string(files[0].Content))
}

// A reference given by a relative path is stored by git as an absolute one,
// and a reference that is itself a reference clone chains. Refs: HSWE-47.6
func TestCommittedContent_ReferenceCloneOfAReferenceClone(t *testing.T) {
	source := gitRepoWithCommit(t, "file.txt", "chained\n")
	base := filepath.Join(t.TempDir(), "base")
	alternatesGit(t, source, "clone", "--bare", source, base)
	first := referenceClone(t, source, base)
	second := referenceClone(t, source, filepath.Join(first, ".git"))
	files, _, err := CommittedFiles(second)
	require.NoError(t, err)
	require.Len(t, files, 1)
	assert.Equal(t, "chained\n", string(files[0].Content))
}

// A reference store that has been deleted is named in the refusal. Refs: HSWE-47.6
func TestCommittedContent_ReferenceCloneWithReferenceRemoved(t *testing.T) {
	source := gitRepoWithCommit(t, "file.txt", "gone\n")
	reference := filepath.Join(t.TempDir(), "reference")
	alternatesGit(t, source, "clone", "--bare", source, reference)
	clone := referenceClone(t, source, reference)
	require.NoError(t, os.Rename(reference, reference+".moved"))
	_, err := CommittedBlobs(clone)
	require.Error(t, err)
	assert.Contains(t, err.Error(), reference)
}

func statLoose(clone, hash string) error {
	_, err := os.Stat(filepath.Join(clone, ".git", "objects", hash[:2], hash[2:]))
	return err
}
