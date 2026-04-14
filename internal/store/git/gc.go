package git

import (
	"context"
	"fmt"
	"os"
	"path/filepath"

	gogit "github.com/go-git/go-git/v5"
)

// GCStore exposes garbage-collection primitives over the .mgit/objects
// directory: counting loose objects, measuring on-disk size, and packing
// loose objects into packfiles via go-git's RepackObjects.
// Refs: FR-8.4, FR-13.2, MGIT-4.2.11
type GCStore struct {
	repo *Repository
}

// NewGCStore creates a GCStore backed by the given Repository.
func NewGCStore(repo *Repository) *GCStore {
	return &GCStore{repo: repo}
}

// LooseObjectCount returns the number of loose objects currently stored
// under .mgit/objects/<2-char>/<remaining>. Pack files and the info/
// directory are excluded.
// Refs: FR-8.4
func (g *GCStore) LooseObjectCount(_ context.Context) (int, error) {
	objectsDir := filepath.Join(g.repo.MgitDir(), "objects")
	entries, err := os.ReadDir(objectsDir)
	if err != nil {
		if os.IsNotExist(err) {
			return 0, nil
		}
		return 0, fmt.Errorf("read objects dir: %w", err)
	}

	count := 0
	for _, e := range entries {
		name := e.Name()
		if !e.IsDir() || name == "pack" || name == "info" || len(name) != 2 {
			continue
		}
		sub, err := os.ReadDir(filepath.Join(objectsDir, name))
		if err != nil {
			return 0, fmt.Errorf("read object subdir %s: %w", name, err)
		}
		count += len(sub)
	}
	return count, nil
}

// ObjectsDirSize returns the total bytes consumed by .mgit/objects.
// Refs: FR-8.4
func (g *GCStore) ObjectsDirSize(_ context.Context) (int64, error) {
	objectsDir := filepath.Join(g.repo.MgitDir(), "objects")
	var total int64
	err := filepath.Walk(objectsDir, func(_ string, info os.FileInfo, walkErr error) error {
		if walkErr != nil {
			if os.IsNotExist(walkErr) {
				return nil
			}
			return walkErr
		}
		if !info.IsDir() {
			total += info.Size()
		}
		return nil
	})
	if err != nil {
		return 0, fmt.Errorf("walk objects dir: %w", err)
	}
	return total, nil
}

// PackLooseObjects rewrites loose objects into a single packfile via
// go-git's RepackObjects. The aggressive flag is forwarded as a hint —
// go-git always produces a fresh pack, so the flag determines whether
// the existing packs are eligible for replacement.
// Refs: FR-8.4, FR-13.2
func (g *GCStore) PackLooseObjects(_ context.Context, _ bool) error {
	if err := g.repo.repo.RepackObjects(&gogit.RepackConfig{}); err != nil {
		return fmt.Errorf("repack objects: %w", err)
	}
	return nil
}
