//go:build linux || darwin

package basecache

import (
	"fmt"
	"os"
	"path/filepath"

	"golang.org/x/sys/unix"
)

// holdFile is the lock file under the cache root that composes and prune
// coordinate on. It is never removed: unlinking a lock file while it is held
// would let a later process lock a different inode.
const holdFile = ".hold"

// HoldShared holds the cache shared, blocking while prune holds it. A compose
// holds it from publishing (or finding) an entry until its pin is recorded
// and signed, so prune can never remove an entry between the moment a
// compose relies on it and the moment that reliance is written down.
// Composes hold it together. Refs: MGIT-239
func (c *Cache) HoldShared() (func(), error) { return c.hold(unix.LOCK_SH) }

// HoldExclusive holds the cache exclusively, blocking until no compose holds
// it. Prune holds it to take an entry's verdict for the last time and remove
// it, so the verdict cannot go stale before the removal. Refs: MGIT-239
func (c *Cache) HoldExclusive() (func(), error) { return c.hold(unix.LOCK_EX) }

// hold takes a flock on the cache's hold file. The kernel drops it when the
// process dies, so a crashed compose never wedges prune.
func (c *Cache) hold(how int) (func(), error) {
	if err := c.markRoot(); err != nil {
		return nil, fmt.Errorf("base cache: hold: %w", err)
	}
	f, err := os.OpenFile(filepath.Join(c.root, holdFile), os.O_CREATE|os.O_RDWR, 0o600) //nolint:gosec // fixed name under the cache root
	if err != nil {
		return nil, fmt.Errorf("base cache: hold: %w", err)
	}
	if err := unix.Flock(int(f.Fd()), how); err != nil { //nolint:gosec // a file descriptor fits an int
		_ = f.Close() // the flock failure is what is returned
		return nil, fmt.Errorf("base cache: hold: %w", err)
	}
	return func() { _ = f.Close() }, nil // closing the descriptor drops the flock
}
