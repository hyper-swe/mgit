package basecache

import (
	"errors"
)

// Listed is one published entry found in the cache.
type Listed struct {
	Digest string
	Path   string
}

var errNotYet = errors.New("base cache: pinner records are not built yet")

// RecordPinner records that the repository at hostRoot pins digest.
func (c *Cache) RecordPinner(digest, hostRoot string) error { return errNotYet }

// Pinners returns the recorded pinners of digest.
func (c *Cache) Pinners(digest string) (roots []string, recorded bool, err error) {
	return nil, false, errNotYet
}

// Entries lists every published entry.
func (c *Cache) Entries() ([]Listed, error) { return nil, errNotYet }

// Remove deletes one published entry.
func (c *Cache) Remove(digest string) error { return errNotYet }

// TreeBytes sums the sizes of the regular files under dir.
func TreeBytes(dir string) (int64, error) { return 0, errNotYet }
