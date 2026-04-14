# mgit Squash Algorithm Specification

## Purpose

Define the exact algorithm for squashing multiple micro-commits into a single consolidated commit. This document specifies the mechanical steps, atomicity guarantees, and data integrity requirements for squash operations in mgit.

## Squash Algorithm (Pseudocode)

```
function squash(taskID, options):

  1. VALIDATE_TASK_ID
     - Verify taskID exists in mtix
     - Verify corresponding task branch exists: task/{PROJECT}-{ID}
     - If not found: return ErrTaskNotFound

  2. COLLECT_COMMITS
     - Query task_commits index: SELECT all commits WHERE task_id = taskID
     - Order by committed_at ASC (chronological order)
     - Store in memory: microCommits = [c1, c2, ..., cN]
     - If empty: return ErrNoCommitsForTask

  3. VERIFY_CHAIN
     - Walk parent chain: for i in 1..N:
       - If i == 1:
         - parent_hash = parent of c1
         - Verify parent_hash matches task branch base or HEAD of main
       - Else (i > 1):
         - Verify c[i].parent == c[i-1].hash
         - If not: return ErrCommitChainBroken
     - Purpose: guarantee no commits were deleted or reordered

  4. LOCK_BRANCH
     - Acquire distributed lock on task/{PROJECT}-{ID}
     - Lock key: "mgit:branch:task/{PROJECT}-{ID}"
     - Timeout: 30 seconds (configurable)
     - If lock held by another agent: return ErrBranchLocked, wait or fail

  5. COMPUTE_DIFF
     - baseCommit = c1.parent (the commit before micro-commits started)
     - latestCommit = cN (last micro-commit)
     - Calculate unified diff: diff_unified = git_diff(baseCommit.tree, latestCommit.tree)
     - Result: file_changes = { file_path: {additions, deletions, hunks}, ... }

  6. RESOLVE_CONFLICTS
     - Scan for conflicting changes within the same task:
       - If multiple commits modified the same file region:
         - Apply "last-writer-wins": keep the final state from latestCommit
         - Log: "Resolved conflict in {file} at line {N}: keeping latest version"
     - Purpose: ensure squashed tree is deterministic and matches final state
     - If inter-task conflicts detected (rare): return ErrInterTaskConflict, abort

  7. BUILD_TREE
     - Start from baseCommit.tree
     - Apply cumulative diff_unified (from step 5):
       - For each file in diff:
         - If deletion: remove file from tree
         - If modification: update file blob hash with new content
         - If addition: add file blob hash to tree
     - Compute final tree hash via go-git
     - Verify: sha(tree) must be identical to latestCommit.tree (sanity check)

  8. CREATE_COMMIT
     - Build commit object:
       {
         tree_hash: <from step 7>,
         parent: baseCommit.hash,
         author: { name: "mgit-squash", email: "mgit@local" },
         committer: { name: "mgit-squash", email: "mgit@local" },
         committed_at: now() (ISO-8601 UTC),
         message: <see "Message Format" section>,
         gpg_signature: optional (if --sign flag set),
         metadata: {
           task_id: taskID,
           micro_commit_count: N,
           micro_commit_hashes: [c1.hash, c2.hash, ..., cN.hash],
           squash_timestamp: now(),
           agent_id: <from context>,
           base_commit: baseCommit.hash,
           diff_stats: { files_changed: K, insertions: M, deletions: L }
         }
       }
     - Write commit object to go-git object store
     - Commit hash: squashCommit.hash

  9. UPDATE_INDEX
     - Append to task_commits table:
       INSERT INTO task_commits VALUES (
         squashCommit.hash,
         taskID,
         'squash',
         squashCommit.message,
         now(),
         microCommits.map(c => c.hash).to_json_array()
       )
     - This is APPEND-only — original micro-commit records remain
     - Purpose: audit trail of all commits and squash operations

  10. MERGE_TO_MAIN (if --to-main flag)
      - Attempt fast-forward merge: task/{PROJECT}-{ID} → main
      - Algorithm:
        - Check if main is ancestor of squashCommit
        - If YES: update main ref to squashCommit.hash (fast-forward)
        - If NO: return ErrMainDiverged, abort, suggest rebase
        - If EQUAL: main already at squashCommit, return no-op

  11. UNLOCK_BRANCH
      - Release distributed lock on task/{PROJECT}-{ID}
      - Even if steps 1-10 fail, unlock must succeed

  12. RETURN
      - Return: { squash_commit_hash, micro_commit_count, message, main_updated }
```

## Atomicity Guarantee

**Critical invariant: Either all steps succeed, or all roll back. Partial squash = data corruption.**

### Atomic region: Steps 7-10

These steps MUST complete atomically:
1. Build and write git tree
2. Create and write git commit
3. Update task_commits index
4. Update main reference (if --to-main)

**Implementation:**
- **Git atomicity**: go-git provides atomic tree + commit writes (uses git object store semantics)
- **Index atomicity**: SQLite transaction wrapping step 9
- **Main ref atomicity**: go-git reference update is atomic

**Failure handling:**

If any step in the atomic region fails:
1. Detect failure at step N
2. Rollback SQLite transaction (step 9 undone)
3. Git objects may exist on disk but are unreachable (dangling objects)
4. Return ErrSquashFailed with context
5. **Recovery**: User can re-run `mgit squash --task {ID}` — idempotency check catches this and returns existing squash commit hash

If Git write succeeds but SQLite write fails:
- Git commit object exists but is orphaned (unreachable from refs)
- Next `mgit verify --orphans` will detect and report it
- Solution: `mgit cleanup --orphans` or manual re-run of squash (see Idempotence section)

### Lock lifecycle

Lock is acquired at step 4 and released at step 11 (even on failure).

**Why:** Prevents concurrent squash operations on the same branch, which would corrupt the task_commits index.

**Lock timeout:**
- Default: 30 seconds
- If agent crashes: lock auto-expires after timeout
- Config: `mgit.locks.timeout_seconds`

## Message Format for Squashed Commit

```
squash({taskID}): {consolidated_summary}

Squashed {N} micro-commits for task {taskID}.

Micro-commits:
  {hash1_short} {c1.message_subject}
  {hash2_short} {c2.message_subject}
  ...
  {hashN_short} {cN.message_subject}

Agent: {agent_id}
Squash-Time: {ISO-8601 UTC}
Diff-Stats: {files_changed} files changed, {insertions} insertions, {deletions} deletions
```

**Example:**
```
squash(PROJ-4.2.1): Implement OAuth2 flow with token refresh

Squashed 7 micro-commits for task PROJ-4.2.1.

Micro-commits:
  a3f5b9e Add OAuth2 provider interface
  c7d2e1f Implement Google provider
  e8f4a2c Add token refresh logic
  f9g5b3d Fix refresh token expiration
  g0h6c4e Add tests for OAuth2 flow
  h1i7d5f Update configuration schema
  i2j8e6g Documentation for OAuth2 setup

Agent: claude-opus-4.6-agent-id
Squash-Time: 2026-03-09T15:42:00Z
Diff-Stats: 12 files changed, 487 insertions, 23 deletions
```

## --to-git Export

When `mgit squash --task PROJ-4.2.1 --to-git` is invoked (without --to-main):

### Behavior:
1. Perform squash algorithm as normal (steps 1-9)
2. Generate a standard git patch file (compatible with `git apply` and `git am`)
3. Write patch to: `./mgit-exports/{taskID}.patch`
4. Optionally apply patch to external git repo (if configured)

### Patch file format:
```
From: mgit-squash <mgit@local>
Date: {ISO-8601 UTC}
Subject: squash(PROJ-4.2.1): Implement OAuth2 flow with token refresh

Micro-Trace: mgit:{squashCommitHash}

<unified diff content>

--
mgit squash export | {squashCommitHash} | task/PROJ-4.2.1
```

### External repo integration:
- Config: `mgit.export.git_repo = "/path/to/external/repo"`
- If set, after patch generation:
  - Change to external repo directory
  - Run: `git am {patch_file}`
  - If apply succeeds: log success
  - If apply fails: log error, continue (don't fail the squash)

### Use case:
- Testing mgit squash outputs in a standard git environment
- Exporting micro-commits to upstream git repository
- Hybrid workflows (mgit + traditional git)

## Idempotence

**Property: Running squash twice on the same task with no new micro-commits returns the same result.**

### Algorithm:
```
function squash_idempotent(taskID):

  1. Query: existing_squash = SELECT * FROM task_commits
                             WHERE task_id = taskID AND operation = 'squash'

  2. If existing_squash found:
     - Check if new commits added since last squash:
       new_commits = SELECT * FROM task_commits
                     WHERE task_id = taskID AND committed_at > existing_squash.committed_at

     - If no new commits:
       return existing_squash.hash (no-op, same hash)

     - If new commits exist:
       proceed with normal squash algorithm (creates new squash commit)

  3. If no existing squash:
     proceed with normal squash algorithm (first-time squash)
```

### Example:
```bash
# First squash
$ mgit squash --task PROJ-4.2.1
Squashed 7 micro-commits.
Squash commit: a3f5b9e1c2d3e4f5...

# No new commits added

# Second squash
$ mgit squash --task PROJ-4.2.1
No new micro-commits since last squash.
Returning existing squash: a3f5b9e1c2d3e4f5...

# New commits added by agent

# Third squash
$ mgit squash --task PROJ-4.2.1
Squashed 3 new micro-commits (original 7 + 3 new).
Squash commit: f9g5b3d7e8f9a0b1... (new hash, includes all 10)
```

## Error Handling

| Error | Cause | Recovery |
|-------|-------|----------|
| ErrTaskNotFound | Task ID invalid or issue deleted | Verify task ID with mtix |
| ErrNoCommitsForTask | Task branch exists but empty | Add commits via `mgit commit` |
| ErrCommitChainBroken | Commits out of order or deleted | Manual inspection of task_commits index |
| ErrBranchLocked | Another agent squashing same task | Wait for lock timeout or coordinate with agent |
| ErrMainDiverged | main has new commits not on task branch | Rebase task branch onto main, retry |
| ErrSquashFailed | Generic failure during atomic region | Re-run squash (idempotency handles recovery) |
| ErrInterTaskConflict | Commits from different tasks mixed (should not happen) | Manual review of task_commits index, contact support |

## Performance Considerations

- **Micro-commits count**: Squash time scales linearly with number of micro-commits (O(N))
- **File count**: Scales linearly with number of changed files (O(M))
- **Typical performance**: 100 micro-commits squashing ~50 files takes < 1 second
- **Bottleneck**: Tree diff computation (handled by go-git optimizations)

## Summary

The squash algorithm ensures:
1. **Deterministic**: Same input always produces same squash commit
2. **Atomic**: All or nothing — no partial squashes
3. **Auditable**: Original micro-commits remain in task_commits index
4. **Idempotent**: Running twice on unchanged task is safe
5. **Traceable**: Squash commit includes full micro-commit lineage
