package gitref

import (
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"

	billy "github.com/go-git/go-billy/v5"
	"github.com/go-git/go-git/v5/plumbing/cache"
	"github.com/go-git/go-git/v5/storage/filesystem"
)

// packReadFS supplies read-only aliases for EVERY indexed Git pack, including
// maintenance's loose-*.pack and arbitrary basenames. go-git discovers only
// pack-<checksum>.pack and validates that checksum against the index trailer.
// No user files are renamed, copied, or written. Refs: FEAT-3.153, MGIT-14
// This view is only for external Git reads, not mgit's writable object store.
type packReadFS struct {
	billy.Filesystem
	aliases map[string]string
	packs   []os.FileInfo
}

type packAliasInfo struct {
	os.FileInfo
	name string
}

func (info packAliasInfo) Name() string { return info.name }

func newPackReadFS(base billy.Filesystem) (*packReadFS, error) {
	fs := &packReadFS{Filesystem: base, aliases: make(map[string]string)}
	dir := filepath.Join("objects", "pack")
	infos, err := base.ReadDir(dir)
	if os.IsNotExist(err) {
		return fs, nil
	}
	if err != nil {
		return nil, err
	}
	sort.Slice(infos, func(i, j int) bool { return infos[i].Name() < infos[j].Name() })
	byName := make(map[string]os.FileInfo, len(infos))
	for _, info := range infos {
		byName[info.Name()] = info
	}
	for _, info := range infos {
		if info.IsDir() || !strings.HasSuffix(info.Name(), ".pack") {
			continue
		}
		stem := strings.TrimSuffix(info.Name(), ".pack")
		idx, paired := byName[stem+".idx"]
		if !paired || idx.IsDir() {
			continue
		}
		f, err := base.Open(filepath.Join(dir, stem+".idx"))
		if err != nil {
			return nil, err
		}
		// The last 40 index bytes are pack checksum, then index checksum.
		var checksum [20]byte
		_, err = f.Seek(-40, io.SeekEnd)
		if err == nil {
			_, err = io.ReadFull(f, checksum[:])
		}
		closeErr := f.Close()
		if err != nil {
			return nil, fmt.Errorf("read pack index %q: %w", stem+".idx", err)
		}
		if closeErr != nil {
			return nil, closeErr
		}
		alias := "pack-" + hex.EncodeToString(checksum[:])
		// Identical indexed packs share a checksum; prefer the canonical name.
		key := filepath.Join(dir, alias+".pack")
		if _, exists := fs.aliases[key]; exists && stem != alias {
			continue
		}
		for _, ext := range []string{".pack", ".idx", ".rev"} {
			virtual := filepath.Join(dir, alias+ext)
			delete(fs.aliases, virtual)
			if actual, ok := byName[stem+ext]; ok && !actual.IsDir() {
				fs.aliases[virtual] = filepath.Join(dir, stem+ext)
			}
		}
	}
	for virtual, actual := range fs.aliases {
		fs.packs = append(fs.packs, packAliasInfo{FileInfo: byName[filepath.Base(actual)], name: filepath.Base(virtual)})
	}
	sort.Slice(fs.packs, func(i, j int) bool { return fs.packs[i].Name() < fs.packs[j].Name() })
	return fs, nil
}

func (fs *packReadFS) readPath(path string) string {
	if actual, ok := fs.aliases[filepath.Clean(path)]; ok {
		return actual
	}
	return path
}
func (fs *packReadFS) Open(path string) (billy.File, error) {
	return fs.Filesystem.Open(fs.readPath(path))
}
func (fs *packReadFS) Stat(path string) (os.FileInfo, error) {
	return fs.Filesystem.Stat(fs.readPath(path))
}

// No write is redirected to a user's Git pack.
func (fs *packReadFS) OpenFile(path string, flag int, _ os.FileMode) (billy.File, error) {
	if flag == os.O_RDONLY {
		return fs.Open(path)
	}
	return nil, billy.ErrReadOnly
}
func (fs *packReadFS) ReadDir(path string) ([]os.FileInfo, error) {
	if filepath.Clean(path) != filepath.Join("objects", "pack") {
		return fs.Filesystem.ReadDir(path)
	}
	return append([]os.FileInfo(nil), fs.packs...), nil
}

// The external Git view cannot be used to mutate its backing filesystem.
func (fs *packReadFS) Create(string) (billy.File, error)           { return nil, billy.ErrReadOnly }
func (fs *packReadFS) TempFile(string, string) (billy.File, error) { return nil, billy.ErrReadOnly }
func (fs *packReadFS) Rename(string, string) error                 { return billy.ErrReadOnly }
func (fs *packReadFS) Remove(string) error                         { return billy.ErrReadOnly }
func (fs *packReadFS) MkdirAll(string, os.FileMode) error          { return billy.ErrReadOnly }
func (fs *packReadFS) Symlink(string, string) error                { return billy.ErrReadOnly }
func (fs *packReadFS) Capabilities() billy.Capability {
	return billy.ReadCapability | billy.SeekCapability
}
func (fs *packReadFS) Chroot(path string) (billy.Filesystem, error) {
	child, err := fs.Filesystem.Chroot(path)
	if err != nil {
		return nil, err
	}
	return newPackReadFS(child)
}

// NewReadOnlyStorage adapts a filesystem's indexed packs for external Git
// object readers. The caller supplies the reference-aware filesystem so a
// linked worktree retains its own HEAD and its common object/ref directory.
// Refs: FEAT-3.153, MGIT-14
func NewReadOnlyStorage(base billy.Filesystem) (*filesystem.Storage, error) {
	readFS, err := newPackReadFS(base)
	if err != nil {
		return nil, err
	}
	return filesystem.NewStorage(readFS, cache.NewObjectLRUDefault()), nil
}
