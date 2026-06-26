package git

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
)

// syncStateName is the file under .mgit/ that records the fingerprint the base
// was last synced from. It lives in the SHARED store directory (alongside
// objects/refs/index.db), so all linked worktrees of one store read/write the
// same drift signal under the same file lock. Refs: MGIT-35, ADR-008 §3
const syncStateName = "sync_state.json"

// SyncState is the persisted drift signal: the local git HEAD commit id and the
// working-tree content fingerprint the `.mgit` base was last synced from, plus
// the base commit the resync advanced the base branch to. The gate recomputes
// the live (HeadCommit, WorkTreeHash) and resyncs ONLY when either differs —
// the cheap common path. Refs: MGIT-35, ADR-008 §3
type SyncState struct {
	// GitHead is the project's local git HEAD commit id at last sync (read
	// READ-ONLY from .git; empty when the project has no readable git).
	GitHead string `json:"git_head"`
	// WorkTreeHash is the working-tree content fingerprint at last sync.
	WorkTreeHash string `json:"worktree_hash"`
	// BaseCommit is the .mgit base-branch commit the last resync produced.
	BaseCommit string `json:"base_commit"`
	// SyncedAt is an ISO-8601 UTC timestamp for diagnostics only.
	SyncedAt string `json:"synced_at"`
}

// syncStatePath returns the absolute path of the sync-state file in the SHARED
// store directory (MgitDir), so worktrees and the parent agree on one signal.
func (r *Repository) syncStatePath() string {
	return filepath.Join(r.MgitDir(), syncStateName)
}

// ReadSyncState loads the persisted drift signal. A missing file yields a zero
// SyncState with found=false (never synced yet) — NOT an error. A present but
// corrupt file is a hard error rather than a silent reset, so a damaged signal
// never causes a missed resync. Refs: MGIT-35, ADR-008 §3
func (r *Repository) ReadSyncState() (SyncState, bool, error) {
	data, err := os.ReadFile(r.syncStatePath()) //nolint:gosec // internal path under .mgit
	if errors.Is(err, os.ErrNotExist) {
		return SyncState{}, false, nil
	}
	if err != nil {
		return SyncState{}, false, fmt.Errorf("read sync state: %w", err)
	}
	var s SyncState
	if err := json.Unmarshal(data, &s); err != nil {
		return SyncState{}, false, fmt.Errorf("parse sync state %s: %w", r.syncStatePath(), err)
	}
	return s, true, nil
}

// WriteSyncState persists the drift signal ATOMICALLY (write to a temp file in
// the same directory, then rename) so an interrupted write can never leave a
// half-written, unparseable signal. Refs: MGIT-35, ADR-008 §6
func (r *Repository) WriteSyncState(s SyncState) error {
	data, err := json.MarshalIndent(s, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal sync state: %w", err)
	}
	dir := r.MgitDir()
	tmp, err := os.CreateTemp(dir, syncStateName+".tmp-*")
	if err != nil {
		return fmt.Errorf("create temp sync state: %w", err)
	}
	tmpName := tmp.Name()
	if _, err := tmp.Write(append(data, '\n')); err != nil {
		_ = tmp.Close()
		_ = os.Remove(tmpName)
		return fmt.Errorf("write temp sync state: %w", err)
	}
	if err := tmp.Close(); err != nil {
		_ = os.Remove(tmpName)
		return fmt.Errorf("close temp sync state: %w", err)
	}
	if err := os.Rename(tmpName, r.syncStatePath()); err != nil {
		_ = os.Remove(tmpName)
		return fmt.Errorf("commit sync state: %w", err)
	}
	return nil
}
