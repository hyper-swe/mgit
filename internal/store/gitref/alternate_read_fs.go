package gitref

import (
	"bufio"
	"fmt"
	"os"
	"path/filepath"

	billy "github.com/go-git/go-billy/v5"
	"github.com/go-git/go-billy/v5/osfs"
)

// alternateReadFS resolves explicit absolute borrowed-store paths independently
// of the clone's chroot. All object reads still pass through packReadFS, so
// borrowed maintenance packs get the same aliases as primary packs.
// Refs: MGIT-294, MGIT-14
// This filesystem is wrapped in packReadFS before it reaches an object reader.
type alternateReadFS struct{ billy.Filesystem }

func (fs *alternateReadFS) Stat(path string) (os.FileInfo, error) {
	if filepath.IsAbs(path) {
		info, err := os.Stat(path)
		if err == nil {
			return info, nil
		}
		// go-git also prefixes relative alternate paths with '/'. Preserve
		// the existing rooted lookup for those; absolute paths are validated
		// before object reads, so unavailable borrowed stores cannot fall back.
		if os.IsNotExist(err) {
			return fs.Filesystem.Stat(path)
		}
		return nil, err
	}
	return fs.Filesystem.Stat(path)
}

func (fs *alternateReadFS) Chroot(path string) (billy.Filesystem, error) {
	if !filepath.IsAbs(path) {
		path = filepath.Join(fs.Root(), path)
	} else if _, err := os.Stat(path); os.IsNotExist(err) {
		child, childErr := fs.Filesystem.Chroot(path)
		if childErr != nil {
			return nil, childErr
		}
		return newPackReadFS(&alternateReadFS{Filesystem: child})
	}
	return newPackReadFS(&alternateReadFS{Filesystem: osfs.New(path)})
}

// validateAbsoluteAlternates checks borrowed stores eagerly because go-git's
// fallback object search discards alternate-directory errors. Name the failed
// store and recovery action instead of returning an unqualified missing object.
// Refs: MGIT-294
func validateAbsoluteAlternates(base billy.Filesystem, active map[string]bool) error {
	f, err := base.Open(filepath.Join("objects", "info", "alternates"))
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return alternateReadError(base.Root(), err)
	}
	defer f.Close() // read-only handle; close cannot change either store
	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		path := scanner.Text()
		if !filepath.IsAbs(path) {
			continue
		} // relative-alternate semantics are unchanged
		path = filepath.Clean(path)
		if active[path] {
			return alternateReadError(path, fmt.Errorf("cyclic alternate object store"))
		}
		if _, err := os.ReadDir(path); err != nil {
			return alternateReadError(path, err)
		}
		active[path] = true
		child := osfs.New(filepath.Dir(path))
		if _, err := newPackReadFS(child); err != nil {
			return alternateReadError(path, err)
		}
		err := validateAbsoluteAlternates(child, active)
		delete(active, path)
		if err != nil {
			return err
		}
	}
	if err := scanner.Err(); err != nil {
		return alternateReadError(base.Root(), err)
	}
	return nil
}

func alternateReadError(path string, err error) error {
	return fmt.Errorf("read alternate object store %q: %w; restore access to the borrowed repository or create a self-contained clone", path, err)
}
