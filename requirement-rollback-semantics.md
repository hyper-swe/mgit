# mgit Rollback Semantics Specification

## Purpose

Define the exact behavior of rollback operations in mgit. This document specifies how rollbacks are performed while maintaining the append-only audit model and ensuring safety in safety-critical systems.

## Core Invariant

**Rollback is an APPEND operation. It never deletes old commits from the object store or the task_commits index.**

Instead, rollback creates a NEW revert commit that undoes the changes. The original commits remain in the history, creating an auditable record of both the action and its reversal.

```
Original state:   [c1]--[c2]--[c3]--[c4]
                                      ↑
After rollback:   [c1]--[c2]--[c3]--[c4]--[revert]
                                            (undoes c3 and c4)
```

All commits (original and revert) appear in task_commits index and git history. The working directory reflects the reverted state, but nothing is hidden or deleted.

## Rollback Modes

### Mode 1: Per-Task Rollback

**Command:**
```bash
mgit rollback --task PROJ-4.2.1.3
```

**Purpose:** Undo all changes made by a specific mtix task (micro-issue).

**Algorithm:**

```
function rollback_task(taskID):

  1. VALIDATE_TASK
     - Verify taskID exists in mtix
     - Verify task has associated commits in task_commits index
     - If not found: return ErrTaskNotFound

  2. COLLECT_COMMITS
     - Query: commits = SELECT * FROM task_commits WHERE task_id = taskID
     - Order chronologically: oldest first
     - If empty: return ErrNoCommitsForTask (nothing to rollback)

  3. CHECK_PREVIOUS_ROLLBACK
     - Query: previous = SELECT * FROM task_commits
                         WHERE task_id = taskID + "_rollback"
                         ORDER BY committed_at DESC LIMIT 1
     - If found: WARN: "Task {taskID} was previously rolled back at {timestamp}.
                 Proceeding will re-apply original changes."
     - Continue (this is allowed by append-only model)

  4. COMPUTE_INVERSE_DIFF
     - baseCommit = commits[0].parent
     - currentCommit = commits[N-1] (last commit in task)
     - Calculate unified diff: diff = git_diff(baseCommit.tree, currentCommit.tree)
     - Invert diff: inverse_diff = negate(diff)
       - Additions become deletions
       - Deletions become additions
       - Modifications reversed
     - Result: file_changes = { file_path: {inverse_hunks}, ... }

  5. BUILD_REVERT_TREE
     - Start from currentCommit.tree
     - Apply inverse_diff (from step 4):
       - For each file in inverse_diff:
         - If deletion in inverse: remove from tree
         - If modification in inverse: apply modification
         - If addition in inverse: add to tree
     - Compute final tree hash
     - Verify: sha(tree) should match baseCommit.tree (sanity check)

  6. CREATE_REVERT_COMMIT
     - Build commit object:
       {
         tree_hash: <from step 5>,
         parent: currentCommit.hash,
         author: { name: "mgit-rollback", email: "mgit@local" },
         committer: { name: "mgit-rollback", email: "mgit@local" },
         committed_at: now(),
         message: <see "Message Format" section>,
         metadata: {
           rollback_type: "task",
           original_task_id: taskID,
           reverted_commit_hashes: [commits[0].hash, ..., commits[N-1].hash],
           reverted_count: N,
           rollback_timestamp: now(),
           agent_id: <from context>,
           reason: <optional user-provided reason>
         }
       }
     - Write to go-git object store
     - revertCommit.hash = hash of new commit

  7. UPDATE_WORKING_DIRECTORY
     - Checkout revertCommit.tree to working directory
     - Report: "Files reverted: {file_count}"
     - Show diff preview of what changed

  8. UPDATE_INDEX
     - Append to task_commits:
       INSERT INTO task_commits VALUES (
         revertCommit.hash,
         taskID + "_rollback",     # tagged as rollback of original task
         'rollback',
         revertCommit.message,
         now(),
         commits.map(c => c.hash).to_json_array()
       )
     - Purpose: record that this rollback happened, and which commits it reverted

  9. NOTIFY_MTIX
     - Send event to mtix via MCP: { event: "rollback", task_id: taskID, status: "open" }
     - mtix sets task status to "open" (re-opens for rework)
     - Rationale: task was being worked on, now the work is undone, reopen for repair

  10. RETURN
      - Return: { reverted_count, revert_commit_hash, files_affected, message }
```

**Message format:**
```
revert(PROJ-4.2.1.3): rollback task changes

Reverted {N} commits for task PROJ-4.2.1.3.

Reverted commits:
  {hash1_short} {c1.message_subject}
  {hash2_short} {c2.message_subject}
  ...
  {hashN_short} {cN.message_subject}

Files affected: {file_count}
Agent: {agent_id}
Rollback-Time: {ISO-8601 UTC}
Reason: {optional reason provided by user}
```

**Example:**
```
revert(PROJ-4.2.1.3): rollback task changes

Reverted 3 commits for task PROJ-4.2.1.3.

Reverted commits:
  a3f5b9e Fix authentication bug (attempt 1)
  c7d2e1f Fix authentication bug (attempt 2)
  e8f4a2c Add regression test

Files affected: 4
Agent: claude-opus-4.6-agent-id
Rollback-Time: 2026-03-09T16:15:00Z
Reason: Authentication changes caused test failures; reverting for investigation.
```

### Mode 2: Point-in-Time Rollback

**Command:**
```bash
mgit rollback --commit {hash}
```

**Purpose:** Restore working directory to the state at a specific commit without deleting subsequent commits.

**Algorithm:**

```
function rollback_commit(commitHash):

  1. VALIDATE_COMMIT
     - Verify commitHash exists in git object store
     - If not found: return ErrCommitNotFound

  2. GET_TARGET_STATE
     - targetCommit = fetch(commitHash)
     - targetTree = targetCommit.tree

  3. COMPUTE_INVERSE_DIFF
     - currentBranch = HEAD
     - currentCommit = currentBranch.head_commit
     - Calculate: diff = git_diff(commitHash.tree, currentCommit.tree)
     - Invert: inverse_diff = negate(diff)
     - Result: file_changes for rollback

  4. BUILD_REVERT_TREE
     - Start from currentCommit.tree
     - Apply inverse_diff:
       - Each file reverted to state at targetCommit
     - Compute final tree hash
     - Verify: sha(tree) must match targetTree

  5. CREATE_REVERT_COMMIT
     - Build commit:
       {
         tree_hash: <from step 4>,
         parent: currentCommit.hash,
         author: { name: "mgit-rollback", email: "mgit@local" },
         committer: { name: "mgit-rollback", email: "mgit@local" },
         committed_at: now(),
         message: <see "Message Format" section>,
         metadata: {
           rollback_type: "point_in_time",
           target_commit: commitHash,
           rollback_timestamp: now(),
           agent_id: <from context>,
           reason: <optional>
         }
       }
     - Write to object store
     - revertCommit.hash = new commit hash

  6. UPDATE_WORKING_DIRECTORY
     - Checkout revertCommit.tree to working directory
     - Report files changed

  7. UPDATE_INDEX
     - Append to task_commits:
       INSERT INTO task_commits VALUES (
         revertCommit.hash,
         "point_in_time_rollback",
         'rollback',
         revertCommit.message,
         now(),
         [commitHash]  # reference to what we rolled back to
       )

  8. NOTIFY_MTIX
     - Send event to mtix: { event: "rollback_point_in_time", commit_hash: commitHash }
     - mtix does NOT change task status (manual review needed)
     - Rationale: Point-in-time rollback is exploratory, not task-level

  9. RETURN
     - Return: { revert_commit_hash, target_commit, files_affected }
```

**Message format:**
```
revert({hash_short}): rollback to point-in-time

Rolled back to commit {hash_short}.

Previous commit (parent of revert): {prev_hash_short}
Target commit (restored state): {hash_short}

{N} commits "undone" (but still in history):
  {hash[N]} {c[N].subject}
  ...
  {hash[prev]} {c[prev].subject}

Files affected: {file_count}
Agent: {agent_id}
Rollback-Time: {ISO-8601 UTC}
```

### Mode 3: Dry-Run Mode

**Command:**
```bash
mgit rollback --task PROJ-4.2.1.3 --dry-run
```

**Purpose:** Preview what would change without making modifications.

**Behavior:**

```
function rollback_dry_run(taskID):

  1. Execute steps 1-5 from per-task rollback (compute revert tree)
  2. Do NOT execute steps 7-10 (no actual modifications)
  3. Calculate what files would change:
     - files_to_delete = [list]
     - files_to_modify = [list with before/after hunks]
     - files_to_add = [list]
  4. Return preview information WITHOUT modifying anything
```

**Output:**
```bash
$ mgit rollback --task PROJ-4.2.1.3 --dry-run

DRY RUN: No changes will be made.

Commits to revert (3):
  a3f5b9e Fix authentication bug (attempt 1)
  c7d2e1f Fix authentication bug (attempt 2)
  e8f4a2c Add regression test

Files affected (4):
  MODIFIED: src/auth/oauth.go
    - Lines 45-67: remove Google provider logic
    - Lines 120-130: revert token refresh
  MODIFIED: src/auth/config.go
    - Line 15: remove oauth_enabled flag
  MODIFIED: tests/auth_test.go
    - Lines 200-220: remove new test cases
  DELETED: src/auth/oauth_google.go

Diff summary:
  12 lines removed from oauth.go
  8 lines removed from config.go
  21 lines removed from auth_test.go
  1 file deleted

To apply rollback: mgit rollback --task PROJ-4.2.1.3
```

**Use cases:**
- Preview impact before committing to rollback
- Identify conflicts with uncommitted changes
- Verify scope of reversal

## Idempotence

**Property: Rolling back the same task twice creates TWO revert commits.**

This is by design for the append-only model.

### Example:

```
Commit history:
[c1]--[c2]--[c3]

First rollback (mgit rollback --task PROJ-4.2.1.3):
[c1]--[c2]--[c3]--[revert1]  (undoes c2 and c3)
                   ↑
                   tree = c1's tree

Second rollback (same command):
[c1]--[c2]--[c3]--[revert1]--[revert2]
                   ↑           ↑
                   ↓           ↓
                   c1's tree   c3's tree (re-applies c2 and c3)
```

**Warning on repeated rollbacks:**

```bash
$ mgit rollback --task PROJ-4.2.1.3

WARNING: Task PROJ-4.2.1.3 was previously rolled back at 2026-03-09T16:00:00Z.
Proceeding will re-apply the original changes (inverse of the revert).

Continue? [y/N] _
```

**Rationale:**
- Append-only means every action is recorded
- Two rollbacks = "undo, then redo"
- This is sometimes intentional (exploring alternatives)
- Warning prevents accidental re-application

## Working Directory Conflicts

When rolling back, if working directory has uncommitted changes that would be overwritten:

### Default behavior: Abort with conflict

```bash
$ mgit rollback --task PROJ-4.2.1.3

ERROR: Working directory has uncommitted changes that conflict with rollback.

Conflicting files:
  src/auth/oauth.go (MODIFIED)
  src/auth/config.go (MODIFIED)

Options:
  1. Commit changes: git commit -m "..."
  2. Stash changes: git stash
  3. Force rollback: mgit rollback --force (will stash for you)
  4. Merge rollback: mgit rollback --merge (attempt three-way merge)
```

### With --force flag

```bash
mgit rollback --task PROJ-4.2.1.3 --force
```

**Behavior:**
1. Stash uncommitted changes: `git stash create "mgit-rollback-safety-stash"`
2. Perform rollback
3. Log stash location for user recovery: "Stashed uncommitted changes in: stash@{0}"
4. User can later recover with: `git stash pop stash@{0}`

### With --merge flag

```bash
mgit rollback --task PROJ-4.2.1.3 --merge
```

**Behavior:**
1. Attempt three-way merge:
   - Base: revert commit's parent
   - Ours: current uncommitted changes
   - Theirs: revert commit's tree
2. If merge succeeds: apply rollback with conflicts resolved
3. If merge fails: abort with conflict markers in working directory
   - User must manually resolve
   - Then: `mgit rollback --resolve` to complete

## Integration with mtix

Per-task rollback sends notifications to mtix via MCP:

```
Event: rollback_task
Data: {
  task_id: PROJ-4.2.1.3,
  status: "open",          # task reopened for rework
  revert_commit: hash,
  reverted_count: 3,
  reason: <optional>,
  timestamp: ISO-8601
}
```

**mtix response:**
- Sets task status to "open"
- Updates task history with rollback event
- Notifies assigned agent(s) that task was rolled back
- May trigger re-assignment or escalation (depends on mtix policy)

Point-in-time rollback sends:

```
Event: rollback_point_in_time
Data: {
  task_id: null,           # not task-specific
  target_commit: hash,
  revert_commit: hash,
  reason: <optional>,
  timestamp: ISO-8601
}
```

**mtix response:**
- Logs event
- Does not change any task status
- For audit trail only

## Audit Trail

Every rollback is recorded in the append-only audit trail.

### Audit entry format:

```json
{
  "timestamp": "2026-03-09T16:15:00Z",
  "operation": "rollback",
  "agent_id": "claude-opus-4.6-agent-id",
  "rollback_type": "task",
  "affected_task_ids": ["PROJ-4.2.1.3"],
  "reverted_commit_hashes": [
    "a3f5b9e1c2d3e4f5...",
    "c7d2e1f3g4h5i6j7...",
    "e8f4a2c5d6e7f8g9..."
  ],
  "revert_commit_hash": "f9g5b3d7e8f9a0b1...",
  "reason": "Authentication changes caused test failures; reverting for investigation.",
  "files_affected_count": 4,
  "status": "success"
}
```

### Query audit trail:

```bash
# All rollbacks for a task
mgit audit --type rollback --task PROJ-4.2.1.3

# All rollbacks in date range
mgit audit --type rollback --from 2026-03-01 --to 2026-03-10

# Rollbacks by specific agent
mgit audit --type rollback --agent claude-opus-4.6-agent-id

# All operations (commits + rollbacks + squashes)
mgit audit --all
```

## Error Handling

| Error | Cause | Recovery |
|-------|-------|----------|
| ErrTaskNotFound | Task ID invalid or task deleted | Verify task ID in mtix |
| ErrNoCommitsForTask | No commits exist for task | Nothing to rollback, verify task |
| ErrCommitNotFound | Commit hash invalid (point-in-time) | Verify hash with `mgit log` |
| ErrWorkingDirConflict | Uncommitted changes conflict | Use --force, --merge, or commit first |
| ErrRollbackFailed | Generic failure during revert | Retry, contact support if persistent |
| ErrMergeConflict | Three-way merge failed (--merge) | Manually resolve conflicts, `mgit rollback --resolve` |

## Summary

The rollback semantics ensure:
1. **Append-only**: Rollbacks create new commits, never delete old ones
2. **Auditable**: Full history of rollbacks in audit trail
3. **Idempotent**: Running twice on same task creates second revert (by design)
4. **Safe**: Conflicts detected and resolved explicitly
5. **Traceable**: Rollback commits reference original commits
6. **Integrated**: mtix notified, task status updated appropriately
