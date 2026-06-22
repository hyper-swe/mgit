package git

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
)

// worktreeMarkerName is the file inside a linked worktree's .mgit/ directory
// that marks it as a worktree and points at the shared parent store. Its
// presence is how OpenApp distinguishes a linked worktree from a real store.
// Refs: FR-16, MGIT-24, ADR-007
const worktreeMarkerName = "worktree"

// WorktreeMarker records a linked worktree's binding: the absolute path to the
// shared parent .mgit store, the branch the worktree is bound to (its
// per-worktree HEAD), and the task it serves. It is self-contained so OpenApp
// can resolve a worktree without consulting the registry first. Refs: FR-16, MGIT-24
type WorktreeMarker struct {
	Store  string `json:"store"`
	Branch string `json:"branch"`
	Task   string `json:"task"`
}

// WriteWorktreeMarker writes the marker into worktreeRoot/.mgit/worktree,
// recording the shared store (this repository's .mgit), the bound branch, and
// the task. The worktree's .mgit directory is created if needed (it also holds
// the agent shims). Refs: FR-16, MGIT-24
func (ws *WorktreeStore) WriteWorktreeMarker(worktreeRoot, branch, task string) error {
	marker := WorktreeMarker{Store: ws.repo.MgitDir(), Branch: branch, Task: task}
	dir := filepath.Join(worktreeRoot, mgitDirName)
	if err := os.MkdirAll(dir, 0o750); err != nil {
		return fmt.Errorf("worktree marker: mkdir %s: %w", dir, err)
	}
	data, err := json.MarshalIndent(marker, "", "  ")
	if err != nil {
		return fmt.Errorf("worktree marker: marshal: %w", err)
	}
	path := filepath.Join(dir, worktreeMarkerName)
	if err := os.WriteFile(path, append(data, '\n'), 0o600); err != nil {
		return fmt.Errorf("worktree marker: write %s: %w", path, err)
	}
	return nil
}

// ReadWorktreeMarker reads the linked-worktree marker at
// worktreeRoot/.mgit/worktree. The bool is false (with nil error) when no
// marker exists — i.e. worktreeRoot is a normal repository, not a linked
// worktree. A present-but-corrupt marker is a hard error. Refs: FR-16, MGIT-24
func ReadWorktreeMarker(worktreeRoot string) (*WorktreeMarker, bool, error) {
	path := filepath.Join(worktreeRoot, mgitDirName, worktreeMarkerName)
	data, err := os.ReadFile(path) //nolint:gosec // mgit-internal path under .mgit
	if os.IsNotExist(err) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, fmt.Errorf("read worktree marker: %w", err)
	}
	var m WorktreeMarker
	if err := json.Unmarshal(data, &m); err != nil {
		return nil, false, fmt.Errorf("parse worktree marker %s: %w", path, err)
	}
	if m.Store == "" || m.Branch == "" {
		return nil, false, fmt.Errorf("invalid worktree marker %s: missing store or branch", path)
	}
	return &m, true, nil
}
