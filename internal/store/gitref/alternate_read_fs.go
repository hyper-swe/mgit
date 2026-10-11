package gitref

import (
	"bufio"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"

	billy "github.com/go-git/go-billy/v5"
	"github.com/go-git/go-billy/v5/osfs"
	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/storage/filesystem"
)

// ReadOnlyStorage reads primary objects first, consulting borrowed stores only
// when an object is absent locally. A repacked shared clone remains readable
// even if its former source no longer exists. Refs: MGIT-294, MGIT-14
// Embedded writes still encounter packReadFS's read-only filesystem.
type ReadOnlyStorage struct {
	*filesystem.Storage
	base       billy.Filesystem
	mu         sync.Mutex
	alternates map[string]*filesystem.Storage
}

// EncodedObject searches local objects before recursively reading alternates.
// Alternate errors are retained only when no store supplied the requested
// object; an unused, unavailable alternate cannot invalidate local content.
// Refs: MGIT-294, MGIT-14
func (s *ReadOnlyStorage) EncodedObject(kind plumbing.ObjectType, hash plumbing.Hash) (plumbing.EncodedObject, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	obj, err := s.Storage.EncodedObject(kind, hash)
	if !errors.Is(err, plumbing.ErrObjectNotFound) {
		return obj, err
	}
	objects := filepath.Join(s.base.Root(), "objects")
	return s.readAlternates(s.base, objects, kind, hash, map[string]bool{objects: true})
}

// HasEncodedObject includes objects borrowed from alternates. Refs: MGIT-294
func (s *ReadOnlyStorage) HasEncodedObject(hash plumbing.Hash) error {
	_, err := s.EncodedObject(plumbing.AnyObject, hash)
	return err
}

// EncodedObjectSize includes objects borrowed from alternates. Refs: MGIT-294
func (s *ReadOnlyStorage) EncodedObjectSize(hash plumbing.Hash) (int64, error) {
	obj, err := s.EncodedObject(plumbing.AnyObject, hash)
	if err != nil {
		return 0, err
	}
	return obj.Size(), nil
}

// primaryOnlyFS disables go-git's recursive fallback: it discards errors and
// cannot bound cycles. The read-only wrapper performs that search explicitly.
// Refs: MGIT-294
type primaryOnlyFS struct{ billy.Filesystem }

func (fs *primaryOnlyFS) Open(path string) (billy.File, error) {
	if filepath.Clean(path) == filepath.Join("objects", "info", "alternates") {
		return nil, os.ErrNotExist
	}
	return fs.Filesystem.Open(path)
}

// objectDirectoryFS maps Git's logical objects/ prefix to the directory named
// in alternates, which need not itself be named "objects". Refs: MGIT-294
type objectDirectoryFS struct {
	billy.Filesystem
	directory string
}

func (fs *objectDirectoryFS) objectPath(path string) string {
	path = filepath.Clean(path)
	if path == "objects" {
		return fs.directory
	}
	if strings.HasPrefix(path, "objects"+string(filepath.Separator)) {
		return filepath.Join(fs.directory, strings.TrimPrefix(path, "objects"+string(filepath.Separator)))
	}
	return path
}
func (fs *objectDirectoryFS) Open(path string) (billy.File, error) {
	return fs.Filesystem.Open(fs.objectPath(path))
}
func (fs *objectDirectoryFS) Stat(path string) (os.FileInfo, error) {
	return fs.Filesystem.Stat(fs.objectPath(path))
}
func (fs *objectDirectoryFS) ReadDir(path string) ([]os.FileInfo, error) {
	return fs.Filesystem.ReadDir(fs.objectPath(path))
}

// readAlternates resolves relative paths against the current object database.
// It tries every alternate before returning a retained failure, matching Git's
// ability to read from another store when one configured source is unavailable.
// Refs: MGIT-294
func (s *ReadOnlyStorage) readAlternates(base billy.Filesystem, objects string, kind plumbing.ObjectType, hash plumbing.Hash, active map[string]bool) (plumbing.EncodedObject, error) {
	f, err := base.Open(filepath.Join("objects", "info", "alternates"))
	if os.IsNotExist(err) {
		return nil, plumbing.ErrObjectNotFound
	}
	if err != nil {
		return nil, alternateReadError(objects, err)
	}
	defer f.Close() // read-only handle; closing cannot change either Git store
	var cause error
	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		path := scanner.Text()
		if path == "" {
			continue
		}
		if !filepath.IsAbs(path) {
			path = filepath.Join(objects, path)
		}
		path = filepath.Clean(path)
		obj, readErr := s.readAlternate(path, kind, hash, active)
		if readErr == nil {
			return obj, nil
		}
		if !errors.Is(readErr, plumbing.ErrObjectNotFound) && cause == nil {
			cause = readErr
		}
	}
	if err := scanner.Err(); err != nil {
		return nil, alternateReadError(objects, err)
	}
	if cause != nil {
		return nil, cause
	}
	return nil, plumbing.ErrObjectNotFound
}

func (s *ReadOnlyStorage) readAlternate(path string, kind plumbing.ObjectType, hash plumbing.Hash, active map[string]bool) (plumbing.EncodedObject, error) {
	if _, err := os.ReadDir(path); err != nil {
		return nil, alternateReadError(path, err)
	}
	canonical, err := filepath.EvalSymlinks(path)
	if err != nil {
		return nil, alternateReadError(path, err)
	}
	requested := path
	path = canonical
	if active[path] {
		return nil, alternateReadError(requested, fmt.Errorf("cyclic alternate object store"))
	}

	base := &objectDirectoryFS{Filesystem: osfs.New(filepath.Dir(path)), directory: filepath.Base(path)}
	storage, err := s.alternateStorage(path, base)
	if err != nil {
		return nil, alternateReadError(path, err)
	}
	obj, err := storage.EncodedObject(kind, hash)
	if !errors.Is(err, plumbing.ErrObjectNotFound) {
		if err != nil {
			return nil, alternateReadError(path, err)
		}
		return obj, nil
	}
	active[path] = true
	defer delete(active, path)
	return s.readAlternates(base, path, kind, hash, active)
}

func alternateReadError(path string, err error) error {
	return fmt.Errorf("read alternate object store %q: %w; restore access to the borrowed repository or create a self-contained clone", path, err)
}

// alternateStorage retains decoded pack indexes across borrowed object reads.
// The caller holds s.mu, covering construction and filesystem storage caches.
// Refs: MGIT-294
func (s *ReadOnlyStorage) alternateStorage(path string, base billy.Filesystem) (*filesystem.Storage, error) {
	if storage, ok := s.alternates[path]; ok {
		return storage, nil
	}
	storage, err := newPrimaryReadStorage(base)
	if err != nil {
		return nil, err
	}
	if s.alternates == nil {
		s.alternates = make(map[string]*filesystem.Storage)
	}
	s.alternates[path] = storage
	return storage, nil
}
