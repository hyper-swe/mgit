package git

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/hyper-swe/mgit/internal/model"
)

// boundTaskFileName is the task binding a sandbox guest's private store
// carries, under the store directory the guest sees as its worktree's .mgit.
//
// On the host a linked worktree's .mgit is a marker naming the shared store
// and the bound task, and every command reads the task from it (ADR-007). The
// guest's .mgit is the private store instead (SEC-03), and the marker cannot
// be carried in: it names the host's shared store, which the guest must never
// resolve. This file carries the task alone, so a guest `mgit commit`
// inherits it as a host one does (MGIT-256).
const boundTaskFileName = "bound-task"

// maxBoundTaskBytes caps the read: the file holds one task ID and a newline.
const maxBoundTaskBytes = 256

// WriteBoundTaskInStore records taskID as the task the store directory
// storeDir is bound to. The provisioner calls it for a sandbox's private
// store; it holds the task ID and nothing else, never a host path.
// Refs: MGIT-256, SEC-03, FR-16
func WriteBoundTaskInStore(storeDir, taskID string) error {
	if _, err := model.ParseTaskID(taskID); err != nil {
		return fmt.Errorf("bound task: %w", err)
	}
	final := filepath.Join(storeDir, boundTaskFileName)
	tmp := final + ".tmp"
	if err := os.WriteFile(tmp, []byte(taskID+"\n"), 0o600); err != nil {
		return fmt.Errorf("bound task: write: %w", err)
	}
	if err := os.Rename(tmp, final); err != nil {
		return fmt.Errorf("bound task: commit write: %w", err)
	}
	return nil
}

// ReadBoundTaskInStore returns the task the store directory storeDir is
// bound to, or "" when it records none (every store but a sandbox's private
// one). A record that is not a regular file, is oversized, or does not hold a
// valid task ID is an error: a store that says it is bound but cannot say to
// what must not let a commit fall back to another attribution.
// Refs: MGIT-256, FR-16
func ReadBoundTaskInStore(storeDir string) (string, error) {
	path := filepath.Join(storeDir, boundTaskFileName)
	info, err := os.Lstat(path)
	if err != nil {
		if os.IsNotExist(err) {
			return "", nil
		}
		return "", fmt.Errorf("inspect bound task %s: %w", path, err)
	}
	if !info.Mode().IsRegular() {
		return "", fmt.Errorf("the bound task %s is not a regular file (%s)", path, info.Mode().Type())
	}
	f, err := os.Open(path) //nolint:gosec // fixed store-local path, checked regular above
	if err != nil {
		return "", fmt.Errorf("open bound task %s: %w", path, err)
	}
	defer f.Close() //nolint:errcheck // read-only file, close error is non-actionable
	data, err := io.ReadAll(io.LimitReader(f, maxBoundTaskBytes+1))
	if err != nil {
		return "", fmt.Errorf("read bound task %s: %w", path, err)
	}
	if len(data) > maxBoundTaskBytes {
		return "", fmt.Errorf("the bound task %s is over %d bytes", path, maxBoundTaskBytes)
	}
	id := strings.TrimSpace(string(data))
	if _, err := model.ParseTaskID(id); err != nil {
		return "", fmt.Errorf("the bound task %s: %w", path, err)
	}
	return id, nil
}
