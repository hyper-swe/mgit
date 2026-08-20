package model

import "time"

// Snapshot triggers. A snapshot records WHY the system took it, so a reader
// never has to infer whether a human, an agent, or a timer caused it.
// Refs: MGIT-110, R-H234
const (
	// SnapshotTriggerQuiescence: the worktree changed and then stopped
	// changing, so the system captured the settled state. This is the passive
	// trigger — it requires no agent discipline at all, which is the entire
	// point: recovery must not depend on the agent having been diligent.
	SnapshotTriggerQuiescence = "quiescence"
	// SnapshotTriggerManual: a human or tool asked for a capture explicitly.
	SnapshotTriggerManual = "manual"
)

// Snapshot is a PASSIVE, system-recorded capture of a task worktree's state.
//
// It is deliberately NOT a commit in the agent's narrative. R-H234 separates
// what the SYSTEM KNOWS (the worktree held these bytes at this time) from what
// the AGENT CLAIMS (this was a coherent step), because blending them produces
// a single confident-sounding record that cannot be audited. Three properties
// follow, and they are why this beats any commit-cadence rule:
//
//  1. Recovery stops depending on virtue. An interrupted agent is recoverable
//     whether or not it checkpointed — the MGIT-109 case, where thirty minutes
//     of work survived only as loose files and mgit contributed nothing.
//  2. Provenance cannot be forged by packaging. An end-burst of commits can
//     still be authored, but it can no longer masquerade as process history,
//     because the system holds its own record of when work actually changed.
//  3. A reviewer cannot mistake one for the other — PROVIDED the separation is
//     structural. It is: snapshots are orphan commits under their own ref
//     namespace, unreachable from any task branch, so squash and land cannot
//     include them by construction rather than by rule.
//
// Refs: MGIT-110, MGIT-109, R-H234, MGIT-28
type Snapshot struct {
	ID          string    `json:"id"`          // ULID, sortable by capture time
	TaskID      string    `json:"task_id"`     // task whose worktree this is
	CommitHash  string    `json:"commit_hash"` // the orphan snapshot commit (SHA-1)
	TreeHash    string    `json:"tree_hash"`   // captured tree (SHA-1)
	Fingerprint string    `json:"fingerprint"` // content fingerprint of the worktree
	CapturedAt  time.Time `json:"captured_at"` // when the system captured it
	FileCount   int       `json:"file_count"`  // files in the captured tree
	Trigger     string    `json:"trigger"`     // why it was taken
}
