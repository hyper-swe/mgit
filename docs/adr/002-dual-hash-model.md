# ADR-002: Dual-Hash Model (SHA-1 + SHA-256)

**Status:** Accepted
**Date:** 2026-03-09
**Refs:** NFR-5.1, NFR-5.1a, FR-3.1, FR-12.3

---

## Context

mgit's specification originally stated "SHA-256 for all commit identifiers" (NFR-5.1). However, go-git v5 — the embedded git engine selected in ADR-001 — uses **SHA-1** for git object IDs. This is inherent to the git object format. SHA-256 support in git itself is experimental (introduced in git 2.29, opt-in, and not interoperable with SHA-1 repositories). go-git v5 does not support SHA-256 object IDs in production.

This creates a conflict: the specification mandates SHA-256, but the underlying engine mandates SHA-1.

## Decision

mgit adopts a **dual-hash model**:

| Hash | Algorithm | Purpose | Source |
|------|-----------|---------|--------|
| Git object ID | SHA-1 | Structural addressing within go-git object store | go-git native (unavoidable) |
| `content_hash` | SHA-256 | mgit integrity verification, tamper detection | mgit application layer |

### How it works

1. **Git object IDs (SHA-1)** are used for all go-git operations: object lookup, reference pointers, parent chain navigation, tree addressing. These are produced by go-git and cannot be changed without forking the library.

2. **Content hash (SHA-256)** is a separate field in the mgit commit data model (FR-3.1). It is computed by mgit as: `SHA-256(commit_message + file_diffs_json + task_id + parent_content_hash + created_at)`. This hash is stored in the `task_commits` SQLite index alongside the git SHA-1 object ID.

3. **Integrity verification** (`mgit verify`) checks both:
   - SHA-1: recompute from git object content, compare to stored object ID
   - SHA-256: recompute from mgit metadata, compare to stored content_hash

4. **Audit trail references** use SHA-256 content_hash as the canonical identifier for regulatory compliance (DO-178C, IEC 62304). Git SHA-1 object IDs are treated as internal implementation details.

## Rationale

### Why not wait for SHA-256 in go-git?

- SHA-256 support in git is experimental and opt-in (not default)
- go-git has no timeline for SHA-256 object ID support
- Changing git hash algorithms requires repository migration — a breaking change
- Blocking mgit on this feature would delay development indefinitely

### Why not fork go-git to add SHA-256?

- Forking a critical dependency creates long-term maintenance burden
- go-git is a complex library (~50K lines); a fork would drift quickly
- The dual-hash model achieves SHA-256 integrity without modifying go-git

### Why not use SHA-256 exclusively (custom store)?

- ADR-001 chose go-git specifically for its mature git compatibility
- A custom store would lose git protocol interoperability
- The `--to-git` export feature requires standard git objects

### Is SHA-1 a security risk?

SHA-1 collision attacks (SHAttered, 2017) are a known concern. However:
- Git uses a hardened SHA-1 variant that detects known collision patterns
- mgit's content_hash (SHA-256) provides the cryptographic guarantee
- If a SHA-1 collision is detected, mgit's SHA-256 content_hash will diverge, and `mgit verify` will flag the inconsistency
- The dual-hash model is strictly stronger than SHA-1 alone

## Consequences

### Positive
- SHA-256 integrity guarantee without go-git modification
- Full git protocol compatibility maintained
- `mgit verify` can detect both git-level and mgit-level tampering
- Clear separation of concerns: git handles storage, mgit handles integrity

### Negative
- Two hash computations per commit (negligible performance cost)
- Developers must understand the distinction between the two hash fields
- Commit data model has both `commit_id` (SHA-1) and `content_hash` (SHA-256) fields

### Implementation Notes

```go
// In model/commit.go — both hashes present
type Commit struct {
    CommitID    string    // SHA-1 from go-git (git object ID)
    ContentHash string    // SHA-256 from mgit (integrity hash)
    // ... other fields
}

// In store/git/commit.go — compute content hash after go-git creates commit
func (s *GitStore) CreateCommit(ctx context.Context, ...) (*model.Commit, error) {
    // go-git produces SHA-1 commit ID
    gitCommitHash, err := s.repo.CreateCommit(...)

    // mgit computes SHA-256 content hash
    contentHash := sha256.Sum256([]byte(message + fileDiffsJSON + taskID + parentContentHash + createdAt))

    return &model.Commit{
        CommitID:    gitCommitHash.String(),
        ContentHash: hex.EncodeToString(contentHash[:]),
    }, nil
}
```

---

**Supersedes:** Original NFR-5.1 ("SHA-256 for all commit identifiers")
**Updated by:** NFR-5.1 (revised), NFR-5.1a (new)
