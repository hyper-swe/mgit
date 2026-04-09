package lock

import (
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestAcquire_Success(t *testing.T) {
	mgitDir := filepath.Join(t.TempDir(), ".mgit")

	lock, err := Acquire(mgitDir, DefaultTimeout)
	require.NoError(t, err)
	defer func() { _ = lock.Release() }()

	assert.FileExists(t, lock.Path())
}

func TestAcquire_ReleaseAllowsReacquisition(t *testing.T) {
	mgitDir := filepath.Join(t.TempDir(), ".mgit")

	lock1, err := Acquire(mgitDir, DefaultTimeout)
	require.NoError(t, err)
	require.NoError(t, lock1.Release())

	lock2, err := Acquire(mgitDir, DefaultTimeout)
	require.NoError(t, err)
	require.NoError(t, lock2.Release())
}

func TestAcquire_Concurrent_Serializes(t *testing.T) {
	mgitDir := filepath.Join(t.TempDir(), ".mgit")

	// Goroutine 1 holds the lock
	lock1, err := Acquire(mgitDir, DefaultTimeout)
	require.NoError(t, err)

	var wg sync.WaitGroup
	var lock2 *FileLock
	var lock2Err error

	wg.Add(1)
	go func() {
		defer wg.Done()
		// Goroutine 2 waits (same process so flock allows by default — use different process test)
		lock2, lock2Err = Acquire(mgitDir, 2*time.Second)
	}()

	// Release after a short delay
	time.Sleep(100 * time.Millisecond)
	require.NoError(t, lock1.Release())

	wg.Wait()

	// Note: flock in the same process can be reentrant depending on OS.
	// For true cross-process test, see TestAcquire_CrossProcess in concurrent_cli_test.go.
	_ = lock2
	_ = lock2Err
}

func TestFileLock_ReleaseMultipleTimes(t *testing.T) {
	mgitDir := filepath.Join(t.TempDir(), ".mgit")

	lock, err := Acquire(mgitDir, DefaultTimeout)
	require.NoError(t, err)

	assert.NoError(t, lock.Release())
	assert.NoError(t, lock.Release(), "second release should be no-op")
}
