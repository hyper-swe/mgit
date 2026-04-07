package git

import (
	"fmt"
	"os"
	"path/filepath"

	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/storer"
)

// SyncingStorer wraps a go-git ReferenceStorer with explicit fsync()
// calls after every write operation. This is critical for crash safety
// in safety-critical deployments (hospital, airline, DoD).
// Refs: NFR-3.2, NFR-3.3a, MGIT-2.2.7
type SyncingStorer struct {
	storer.ReferenceStorer
	mgitDir string // path to .mgit/ for fsync targets
}

// NewSyncingStorer wraps a ReferenceStorer with fsync-on-write behavior.
func NewSyncingStorer(inner storer.ReferenceStorer, mgitDir string) *SyncingStorer {
	return &SyncingStorer{
		ReferenceStorer: inner,
		mgitDir:         mgitDir,
	}
}

// SetReference stores a reference and fsyncs the refs directory.
// Refs: NFR-3.3a
func (s *SyncingStorer) SetReference(ref *plumbing.Reference) error {
	if err := s.ReferenceStorer.SetReference(ref); err != nil {
		return err
	}
	return s.syncRefsDir()
}

// syncRefsDir fsyncs the refs/ directory to ensure reference
// updates are persisted to disk before returning success.
func (s *SyncingStorer) syncRefsDir() error {
	refsPath := filepath.Join(s.mgitDir, "refs")
	return syncDir(refsPath)
}

// SyncObjectsDir fsyncs the objects/ directory after object writes.
// Called externally after SetEncodedObject since we can't intercept
// the EncodedObjectStorer interface without reimplementing it.
// Refs: NFR-3.3a
func (s *SyncingStorer) SyncObjectsDir() error {
	objPath := filepath.Join(s.mgitDir, "objects")
	return syncDir(objPath)
}

// syncDir opens a directory and calls fsync on it.
func syncDir(path string) error {
	dir, err := os.Open(path) //nolint:gosec // path is internal, not user input
	if err != nil {
		// Directory may not exist yet (e.g., empty repo)
		if os.IsNotExist(err) {
			return nil
		}
		return fmt.Errorf("open dir for sync %s: %w", path, err)
	}
	defer dir.Close() //nolint:errcheck // best-effort on close after sync

	if err := dir.Sync(); err != nil {
		return fmt.Errorf("fsync dir %s: %w", path, err)
	}
	return nil
}
