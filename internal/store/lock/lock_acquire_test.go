package lock

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestAcquire_SymlinkLockfile_Rejected verifies that Acquire refuses to use
// a lockfile that is a symlink. This defends against symlink attacks where
// an attacker creates a symlink in the .mgit/locks/ directory pointing to
// a sensitive file.
// Refs: NFR-5 (security), MGIT-10.1
func TestAcquire_SymlinkLockfile_Rejected(t *testing.T) {
	mgitDir := filepath.Join(t.TempDir(), ".mgit")
	locksDir := filepath.Join(mgitDir, "locks")
	require.NoError(t, os.MkdirAll(locksDir, 0o700))

	// Create a symlink at the lockfile path pointing to /dev/null.
	lockPath := filepath.Join(locksDir, "mgit.lock")
	require.NoError(t, os.Symlink("/dev/null", lockPath))

	_, err := Acquire(mgitDir, time.Second)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "symlink")
}

// TestAcquire_DirectoryDoesNotExist_CreatesLocksDir verifies that Acquire
// creates the locks directory if it does not exist yet.
func TestAcquire_DirectoryDoesNotExist_CreatesLocksDir(t *testing.T) {
	mgitDir := filepath.Join(t.TempDir(), ".mgit")
	// mgitDir does not exist yet — Acquire should create it via MkdirAll.

	lk, err := Acquire(mgitDir, DefaultTimeout)
	require.NoError(t, err)
	defer func() { _ = lk.Release() }()

	locksDir := filepath.Join(mgitDir, "locks")
	info, err := os.Stat(locksDir)
	require.NoError(t, err)
	assert.True(t, info.IsDir())
}

// TestAcquire_MkdirAllFails_ReturnsError verifies that Acquire returns a
// wrapped error when MkdirAll cannot create the locks directory.
func TestAcquire_MkdirAllFails_ReturnsError(t *testing.T) {
	// Create a regular file where the locks directory would go so MkdirAll fails.
	mgitDir := filepath.Join(t.TempDir(), ".mgit")
	require.NoError(t, os.MkdirAll(mgitDir, 0o700))

	// Plant a regular file at the locks path so MkdirAll fails.
	locksPath := filepath.Join(mgitDir, "locks")
	require.NoError(t, os.WriteFile(locksPath, []byte("blocker"), 0o600))

	_, err := Acquire(mgitDir, time.Second)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "create locks dir")
}

// TestHelperProcess_HoldLock is a helper "test" that is only run as a
// subprocess by tests that need cross-process lock contention. It acquires
// the lock on the directory given in MGIT_LOCK_DIR, prints "LOCKED" to
// stdout, then waits for stdin to be closed before releasing.
func TestHelperProcess_HoldLock(t *testing.T) {
	mgitDir := os.Getenv("MGIT_LOCK_DIR")
	if mgitDir == "" {
		t.Skip("only runs as subprocess")
	}
	lk, err := Acquire(mgitDir, 5*time.Second)
	if err != nil {
		fmt.Fprintf(os.Stderr, "child acquire failed: %v\n", err)
		os.Exit(1)
	}
	fmt.Println("LOCKED")
	// Block until parent closes stdin.
	buf := make([]byte, 1)
	_, _ = os.Stdin.Read(buf)
	_ = lk.Release()
}

// startLockHolder spawns a child process that holds the lock on mgitDir.
// It returns the child process (caller must kill/wait) and a cleanup func.
func startLockHolder(t *testing.T, mgitDir string) *exec.Cmd {
	t.Helper()
	// Re-exec the test binary with just the helper test.
	cmd := exec.Command(os.Args[0], "-test.run=^TestHelperProcess_HoldLock$") //nolint:gosec // standard Go subprocess test pattern using os.Args[0]
	cmd.Env = append(os.Environ(), "MGIT_LOCK_DIR="+mgitDir)
	cmd.Stderr = os.Stderr

	stdout, err := cmd.StdoutPipe()
	require.NoError(t, err)
	stdin, err := cmd.StdinPipe()
	require.NoError(t, err)

	require.NoError(t, cmd.Start())

	// Wait for the child to print "LOCKED".
	buf := make([]byte, 64)
	n, err := stdout.Read(buf)
	require.NoError(t, err)
	require.Contains(t, string(buf[:n]), "LOCKED")

	t.Cleanup(func() {
		_ = stdin.Close()
		_ = cmd.Wait()
	})

	return cmd
}

// TestAcquire_Timeout_WithPID verifies that when another process holds the
// lock and timeout expires, the error includes the holder PID.
// Refs: NFR-3 (reliability)
func TestAcquire_Timeout_WithPID(t *testing.T) {
	mgitDir := filepath.Join(t.TempDir(), ".mgit")

	// Start a child process that holds the lock.
	_ = startLockHolder(t, mgitDir)

	// Try to acquire with a very short timeout — should fail immediately.
	_, err := Acquire(mgitDir, 0)
	require.Error(t, err)
	assert.True(t, errors.Is(err, ErrLockHeld))
	assert.Contains(t, err.Error(), "held by PID")
}

// TestAcquire_Timeout_WithoutPID verifies that when the lock is held but the
// lockfile has no PID written (e.g., truncated), the error still wraps
// ErrLockHeld with a "timeout after" message.
func TestAcquire_Timeout_WithoutPID(t *testing.T) {
	mgitDir := filepath.Join(t.TempDir(), ".mgit")

	// Start a child process that holds the lock.
	_ = startLockHolder(t, mgitDir)

	// Overwrite the lockfile contents with garbage so readPID returns 0.
	lockPath := filepath.Join(mgitDir, "locks", "mgit.lock")
	require.NoError(t, os.WriteFile(lockPath, []byte("not-a-pid"), 0o600))

	_, err := Acquire(mgitDir, 0)
	require.Error(t, err)
	assert.True(t, errors.Is(err, ErrLockHeld))
	assert.Contains(t, err.Error(), "timeout after")
}

// TestReadPID_ValidPID_ReturnsPID verifies readPID reads a valid PID from the lockfile.
func TestReadPID_ValidPID_ReturnsPID(t *testing.T) {
	lockPath := filepath.Join(t.TempDir(), "mgit.lock")
	expectedPID := 42
	require.NoError(t, os.WriteFile(lockPath, []byte(strconv.Itoa(expectedPID)), 0o600))

	pid := readPID(lockPath)
	assert.Equal(t, expectedPID, pid)
}

// TestReadPID_NonexistentFile_ReturnsZero verifies readPID returns 0 when the
// lockfile does not exist.
func TestReadPID_NonexistentFile_ReturnsZero(t *testing.T) {
	pid := readPID(filepath.Join(t.TempDir(), "nonexistent.lock"))
	assert.Equal(t, 0, pid)
}

// TestReadPID_InvalidContents_ReturnsZero verifies readPID returns 0 when
// the lockfile contains non-numeric data.
func TestReadPID_InvalidContents_ReturnsZero(t *testing.T) {
	lockPath := filepath.Join(t.TempDir(), "mgit.lock")
	require.NoError(t, os.WriteFile(lockPath, []byte("not-a-pid"), 0o600))

	pid := readPID(lockPath)
	assert.Equal(t, 0, pid)
}

// TestReadPID_EmptyFile_ReturnsZero verifies readPID returns 0 for an empty lockfile.
func TestReadPID_EmptyFile_ReturnsZero(t *testing.T) {
	lockPath := filepath.Join(t.TempDir(), "mgit.lock")
	require.NoError(t, os.WriteFile(lockPath, []byte(""), 0o600))

	pid := readPID(lockPath)
	assert.Equal(t, 0, pid)
}

// TestErrLockHeld_ErrorFormatting verifies the ErrLockHeld sentinel error
// can be matched with errors.Is and that the formatted timeout messages
// include the expected text.
func TestErrLockHeld_ErrorFormatting(t *testing.T) {
	tests := []struct {
		name     string
		wrapMsg  string
		wantText string
	}{
		{
			name:     "with_pid",
			wrapMsg:  "held by PID 1234 after 5s",
			wantText: "held by PID 1234",
		},
		{
			name:     "timeout_only",
			wrapMsg:  "timeout after 2s",
			wantText: "timeout after 2s",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := fmt.Errorf("%w: %s", ErrLockHeld, tt.wrapMsg)
			assert.True(t, errors.Is(err, ErrLockHeld))
			assert.Contains(t, err.Error(), tt.wantText)
			assert.Contains(t, err.Error(), "another mgit process is running")
		})
	}
}

// TestFileLock_Path_ReturnsLockfilePath verifies that Path() returns the
// correct lockfile path after acquisition.
func TestFileLock_Path_ReturnsLockfilePath(t *testing.T) {
	mgitDir := filepath.Join(t.TempDir(), ".mgit")

	lk, err := Acquire(mgitDir, DefaultTimeout)
	require.NoError(t, err)
	defer func() { _ = lk.Release() }()

	expected := filepath.Join(mgitDir, "locks", "mgit.lock")
	assert.Equal(t, expected, lk.Path())
}

// TestAcquire_WritesCurrentPID verifies that after acquisition the lockfile
// contains the current process PID.
func TestAcquire_WritesCurrentPID(t *testing.T) {
	mgitDir := filepath.Join(t.TempDir(), ".mgit")

	lk, err := Acquire(mgitDir, DefaultTimeout)
	require.NoError(t, err)
	defer func() { _ = lk.Release() }()

	data, err := os.ReadFile(lk.Path())
	require.NoError(t, err)

	pid, err := strconv.Atoi(string(data))
	require.NoError(t, err)
	assert.Equal(t, os.Getpid(), pid)
}

// TestAcquire_OpenFileFails_ReturnsError verifies that when the lockfile
// cannot be opened (e.g., permission denied on locks directory), Acquire
// returns a descriptive error.
func TestAcquire_OpenFileFails_ReturnsError(t *testing.T) {
	mgitDir := filepath.Join(t.TempDir(), ".mgit")
	locksDir := filepath.Join(mgitDir, "locks")
	require.NoError(t, os.MkdirAll(locksDir, 0o700))

	// Remove write permission on the locks directory so OpenFile fails.
	require.NoError(t, os.Chmod(locksDir, 0o500)) //nolint:gosec // intentionally restrictive permissions for test
	t.Cleanup(func() {
		// Restore permissions for cleanup.
		_ = os.Chmod(locksDir, 0o700) //nolint:gosec // restoring permissions after test
	})

	_, err := Acquire(mgitDir, time.Second)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "open lockfile")
}
