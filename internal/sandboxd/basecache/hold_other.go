//go:build !linux && !darwin

package basecache

// HoldShared does nothing here: the guest-base cache serves the sandbox,
// which runs only on Linux and macOS in this version, so no compose and no
// prune contend for it on this platform. Refs: MGIT-239
func (c *Cache) HoldShared() (func(), error) { return func() {}, nil }

// HoldExclusive does nothing here, for the same reason. Refs: MGIT-239
func (c *Cache) HoldExclusive() (func(), error) { return func() {}, nil }
