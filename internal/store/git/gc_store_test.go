package git

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestGCStore_NewGCStore(t *testing.T) {
	repo := initTestRepo(t)
	gc := NewGCStore(repo)
	assert.NotNil(t, gc, "NewGCStore must return non-nil")
}

func TestGCStore_LooseObjectCount_AfterInit(t *testing.T) {
	repo := initTestRepo(t)
	gc := NewGCStore(repo)
	ctx := context.Background()

	count, err := gc.LooseObjectCount(ctx)
	require.NoError(t, err)
	// After init there is at least one commit object, one tree, plus the
	// initial commit signature — exact count depends on go-git internals,
	// but it must be >= 0 and the call must not error.
	assert.GreaterOrEqual(t, count, 0, "loose object count must be non-negative")
}

func TestGCStore_LooseObjectCount_IncreasesWithCommits(t *testing.T) {
	repo := initTestRepo(t)
	gc := NewGCStore(repo)
	ctx := context.Background()

	before, err := gc.LooseObjectCount(ctx)
	require.NoError(t, err)

	// Create a commit to produce new loose objects
	createCommitWithFile(t, repo, "gc_test.go", "package gc\n", "MGIT-1.1")

	after, err := gc.LooseObjectCount(ctx)
	require.NoError(t, err)
	assert.Greater(t, after, before, "commit must create new loose objects")
}

func TestGCStore_ObjectsDirSize_AfterInit(t *testing.T) {
	repo := initTestRepo(t)
	gc := NewGCStore(repo)
	ctx := context.Background()

	size, err := gc.ObjectsDirSize(ctx)
	require.NoError(t, err)
	assert.GreaterOrEqual(t, size, int64(0), "objects dir size must be non-negative")
}

func TestGCStore_ObjectsDirSize_IncreasesWithCommits(t *testing.T) {
	repo := initTestRepo(t)
	gc := NewGCStore(repo)
	ctx := context.Background()

	before, err := gc.ObjectsDirSize(ctx)
	require.NoError(t, err)

	createCommitWithFile(t, repo, "size_test.go", "package size\n", "MGIT-1.1")

	after, err := gc.ObjectsDirSize(ctx)
	require.NoError(t, err)
	assert.Greater(t, after, before, "objects dir size must grow with commits")
}

func TestGCStore_PackLooseObjects_Success(t *testing.T) {
	repo := initTestRepo(t)
	gc := NewGCStore(repo)
	ctx := context.Background()

	// Create several commits to produce loose objects
	for i := range 3 {
		createCommitWithFile(t, repo, "pack_test.go", "package p"+string(rune('0'+i))+"\n", "MGIT-1.1")
	}

	err := gc.PackLooseObjects(ctx, false)
	require.NoError(t, err, "PackLooseObjects must not error")
}

func TestGCStore_PackLooseObjects_Aggressive(t *testing.T) {
	repo := initTestRepo(t)
	gc := NewGCStore(repo)
	ctx := context.Background()

	createCommitWithFile(t, repo, "agg.go", "package agg\n", "MGIT-1.1")

	err := gc.PackLooseObjects(ctx, true)
	require.NoError(t, err, "PackLooseObjects aggressive must not error")
}
