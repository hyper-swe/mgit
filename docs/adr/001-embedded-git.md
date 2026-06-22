# ADR-001: Embedded Git via go-git

## Status
Accepted

## Context

mgit requires an embedded Git engine to provide version control without depending on a system git binary. The engine must be safe, predictable, and have minimal external dependencies.

### Problem Statement
The Git engine must satisfy the following requirements:
- Reliable, deterministic Git operations
- Cross-platform single-binary distribution
- Predictable behavior across operating systems and architectures
- Alignment with safety-critical standards (DO-178C, IEC 62304, NASA-STD-8739.8, MIL-STD-498, OWASP ASVS Level 2)

### Options Considered

1. **go-git v5** (pure Go implementation)
   - Pure Go library, no CGO or C dependencies
   - Cross-platform compilation support
   - Active maintenance and good Go ecosystem integration

2. **libgit2 via git2go** (C bindings)
   - Mature, widely-used C library
   - Feature-complete, well-tested
   - Requires CGO and platform-specific C compilation
   - Adds external dependency complexity

3. **Custom Git implementation**
   - Full control and auditability
   - Significant development burden
   - Risk of reimplementing complex algorithms incorrectly

4. **Shell out to external git binary**
   - Simple integration via subprocess calls
   - Depends on system Git installation
   - Unpredictable behavior across environments
   - Poor for safety-critical systems

## Decision

**Adopt go-git v5** (`github.com/go-git/go-git/v5`) as the embedded Git engine for mgit.

The pure Go implementation produces a single self-contained binary that can be distributed across platforms without external toolchains or runtime dependencies. This satisfies the no-CGO constraint required for safety-critical deployment.

## Rationale

1. **Single Binary Distribution**: Pure Go eliminates CGO build complexity, enabling true cross-platform compilation (amd64, arm64, etc.) without platform-specific toolchains.

2. **Safety-Critical Alignment**: Aligns with DO-178C and IEC 62304 requirements for controlled, auditable dependencies. The absence of a C FFI boundary simplifies static analysis and security review.

3. **Reduced Attack Surface**: Eliminating external process calls and C library code reduces the security surface area. Deterministic behavior is also easier to reason about for both humans and LLM agents.

4. **Ecosystem Integration**: Go's strong typing and error handling improve code safety. Active maintenance and large Go community provide rapid security patches.

## Consequences

### Positive
- **Single self-contained binary**: No CGO toolchain required; simplifies deployment
- **Cross-compilation**: Build for multiple architectures from any platform
- **Auditability**: Pure Go code easier to review statically for safety compliance
- **No external dependencies**: Eliminates system Git installation requirement
- **Concurrency**: Go's goroutine model supports concurrent operations within the process

### Negative
- **Feature completeness**: May lack some advanced Git features (e.g., complex merge strategies, certain plumbing operations)
- **Smaller community**: Fewer users and contributors than libgit2, fewer third-party integrations
- **Maturity**: Slightly younger than libgit2, though actively maintained

### Mitigations
1. **Plumbing API access**: go-git exposes low-level plumbing operations for advanced use cases
2. **Upstream contribution**: Submit patches/PRs for missing features to the go-git project
3. **Feature testing**: Maintain comprehensive test suite covering all operations used by mgit
4. **Fallback documentation**: Document workarounds for unsupported features
5. **Regular audits**: Include go-git in security scanning and dependency reviews per DO-178C

## Implementation Notes

- **Dependency**: Add to `go.mod` with specific version pinning for reproducibility
- **Vendoring**: Consider vendoring go-git and dependencies to meet safety-critical requirements
- **API Layer**: Wrap go-git in a clean internal API to enable future backend swapping if needed
- **Testing**: Unit tests must verify all critical Git operations (clone, fetch, push, merge, status)
- **Error Handling**: Implement comprehensive error handling for safety-critical operations

## Amendment (2026-06-22): mgit coexists with the project's git repository

### Context

This ADR adopted go-git but never specified how mgit's store relates to the
**project's own git repository**. mgit's first-level requirement is that it
runs *inside a git-managed project* and operates **over and above** the
existing git repo — it adds task-tagged micro-commits without owning,
replacing, or disturbing the project's normal git history.

The original `internal/store/git` implementation violated this. `Init`/`Open`
opened the go-git repo with `storage = .mgit/` but passed the **project root
as the go-git worktree**, and the commit/staging/merge/checkout paths were
built on go-git's `Worktree` + index (`wt.Add`/`wt.Commit`/`wt.Checkout`/
`wt.Status`). Because go-git writes a `.git` gitfile at any worktree root,
this:

- in an empty directory, wrote `.git → .mgit`, making plain `git` and `mgit`
  share one store (mgit *hijacks* the `.git` slot); and
- in a real project (which already has a `.git` directory), **failed** at
  `init` with `open <root>/.git: is a directory`, leaving a half-created
  `.mgit` and a cryptic `resolve HEAD: reference not found` on every later
  command.

Net effect: **mgit could not run in any existing git project** — it only
worked in an empty dir. This was missed because every test inits mgit in a
fresh empty `t.TempDir()` (so only the greenfield path ran; line coverage
cannot detect a missing input scenario), mgit was never dogfooded on a real
repo, and the sandbox security audits were scoped to the guest threat model,
not to core git coexistence. The requirement itself was never encoded as a
traceable acceptance criterion.

### Decision

mgit's `.mgit/` store is a **self-contained go-git object store that mgit
drives purely via the plumbing API**, and it **coexists with — never touches
— the project's `.git`**:

1. mgit MUST NOT create, open, read, or mutate the project's `.git` (or write
   any `.git` gitfile at the project root). The project's git history is
   sacrosanct host state.
2. `.mgit/` is opened as a bare/storer-only repo (no go-git worktree), so
   go-git never writes a root `.git`.
3. mgit reads the project's working files via its own filesystem access at
   `Repository.Root()` and builds blob/tree/commit objects via plumbing;
   refs (HEAD, `task/<id>`) are managed directly in `.mgit/`.
4. mgit owns its **own staging model** (mgit-managed, e.g. the SQLite index)
   rather than relying on go-git's `.git/index`.
5. Worktree materialization (FR-16 checkout) writes files via plumbing/osfs,
   not `wt.Checkout()`.

This keeps the go-git decision (pure-Go, plumbing-first per the original
"Plumbing API access" mitigation) and aligns the storage layer with the
coexistence requirement.

### Status of the amendment

Accepted; re-architecture tracked as a dedicated foundational epic (the
commit/staging/merge/checkout + init/open paths are coupled to go-git's
worktree and must move to plumbing). A skipped spec test
(`TestInit_OverExistingGitRepo_*`) encodes the requirement until implemented.
Closure requires: the spec test green, a dogfooding e2e (full mgit lifecycle
over a real git repo with its history left byte-for-byte intact, run in CI),
and an independent review of the "never mutate the project git" invariant.

## References

- [go-git GitHub Repository](https://github.com/go-git/go-git)
- [go-git Documentation](https://pkg.go.dev/github.com/go-git/go-git/v5)
- DO-178C: Software Considerations in Airborne Systems and Equipment Certification
- IEC 62304: Medical Device Software Lifecycle Processes
- NASA-STD-8739.8: Software Safety Standard
- MIL-STD-498: Software Development and Documentation
- OWASP ASVS Level 2: Application Security Verification Standard

## Decision Date
2026-03-09
