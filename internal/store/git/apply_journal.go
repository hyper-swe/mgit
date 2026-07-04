package git

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"github.com/hyper-swe/mgit/internal/model"
)

// applyJournalFile is the crash journal for content-applying commits
// (rollback, cherry-pick — MGIT-54). It is written AFTER the commit object
// exists but BEFORE the branch ref advances, and cleared only once the
// working directory and index are consistent with the new tip. If the
// process dies in between, the next mgit open completes the application
// instead of leaving tip and disk divergent — a divergence the ADR-008
// auto-resync would otherwise absorb, silently undoing the revert/pick.
// Refs: MGIT-54 (review finding H3)
const applyJournalFile = "apply_journal.json"

// ApplyIndexEntry carries what recovery needs to (re-)index the applied
// commit under its task. Refs: MGIT-54
type ApplyIndexEntry struct {
	TaskID  string `json:"task_id"`
	AgentID string `json:"agent_id"`
}

// ApplyJournal is the persisted crash-journal record for one in-flight
// content-applying commit. Root pins the working directory the diffs
// materialize into, so a shared store's journal is only recovered by a
// process opened at the SAME root (a linked worktree's journal is recovered
// by that worktree's next open, never by the parent's). Refs: MGIT-54
type ApplyJournal struct {
	Root        string           `json:"root"`
	CommitHash  string           `json:"commit_hash"`
	ContentHash string           `json:"content_hash"`
	Index       ApplyIndexEntry  `json:"index"`
	Diffs       []model.FileDiff `json:"diffs"`
}

// applyJournalPath returns the journal's location inside the shared store.
func (r *Repository) applyJournalPath() string {
	return filepath.Join(r.MgitDir(), applyJournalFile)
}

// WriteApplyJournal persists the journal atomically (temp + rename). It
// REFUSES to overwrite a pending journal for a DIFFERENT root: that journal
// belongs to an interrupted apply in another worktree, and overwriting it
// would lose that worktree's recovery information. Refs: MGIT-54
func (r *Repository) WriteApplyJournal(j ApplyJournal) error {
	if existing, found, err := r.ReadApplyJournal(); err != nil {
		return err
	} else if found && existing.Root != j.Root {
		return fmt.Errorf("a pending apply for %s awaits recovery; run any mgit command from that directory first", existing.Root)
	}
	data, err := json.MarshalIndent(j, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal apply journal: %w", err)
	}
	tmp := r.applyJournalPath() + ".tmp"
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		return fmt.Errorf("write apply journal: %w", err)
	}
	if err := os.Rename(tmp, r.applyJournalPath()); err != nil {
		return fmt.Errorf("commit apply journal: %w", err)
	}
	return nil
}

// ReadApplyJournal loads the pending journal, reporting found=false when none
// exists. Refs: MGIT-54
func (r *Repository) ReadApplyJournal() (ApplyJournal, bool, error) {
	data, err := os.ReadFile(r.applyJournalPath()) //nolint:gosec // internal store path
	if errors.Is(err, os.ErrNotExist) {
		return ApplyJournal{}, false, nil
	}
	if err != nil {
		return ApplyJournal{}, false, fmt.Errorf("read apply journal: %w", err)
	}
	var j ApplyJournal
	if err := json.Unmarshal(data, &j); err != nil {
		return ApplyJournal{}, false, fmt.Errorf("parse apply journal: %w", err)
	}
	return j, true, nil
}

// ClearApplyJournal removes the journal; clearing an absent journal is a
// no-op. Refs: MGIT-54
func (r *Repository) ClearApplyJournal() error {
	if err := os.Remove(r.applyJournalPath()); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("clear apply journal: %w", err)
	}
	return nil
}
