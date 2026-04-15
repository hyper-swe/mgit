# mgit — Execution Plan

**Version:** 1.1
**Date:** April 2, 2026
**Status:** Active
**Purpose:** Cold-start instructions for LLM agents beginning mgit development

---

## 1. Cold-Start Protocol

If you are an LLM agent starting work on mgit for the first time, follow these steps exactly:

### Prerequisites (one-time setup)

```bash
# Install mtix — the task management system for mgit development
brew install hyper-swe/tap/mtix    # or: go install github.com/hyper-swe/mtix/cmd/mtix@latest

# Configure mtix MCP server for your coding agent (Claude Code example)
claude mcp add mtix -- mtix mcp --project /path/to/your/project

# See CLAUDE.md "TASK MANAGEMENT PREREQUISITES" for other client configs
```

### First Session Steps

```
1. Read THIS FILE (EXECUTION-PLAN.md) — understand wave structure and task pickup
2. Read CLAUDE.md — learn directives, forbidden patterns, verification checklist
3. Read REQUIREMENTS.md — understand FR-1 through FR-16, NFR-1 through NFR-5
4. Read CODING-STYLE.md — learn architecture patterns, go-git conventions, naming
5. Read QUALITY-STANDARDS.md — coverage targets, static analysis, benchmarks
6. Read TDD-WORKFLOW.md — mandatory test-driven process
7. Read APPROVED-PACKAGES.md — the ONLY dependencies you may use
8. Use mtix MCP tools to find your task: mtix_ready → mtix_context → mtix_claim
9. Begin TDD cycle: READ spec → WRITE test → RED → WRITE code → GREEN → REFACTOR
```

**Do NOT skip any step. Do NOT write code before reading the specification.**

---

## 2. Wave-Based Execution Order

Development proceeds in 11 sequential waves. Each wave must be completed before the next begins. Within a wave, tasks may be parallelized where dependencies allow.

### Wave 1: Project Scaffolding (MGIT-1.2)

| Task | Title | Dependencies |
|------|-------|-------------|
| MGIT-1.2.1 | Initialize Go module | None |
| MGIT-1.2.2 | Create project directory structure | MGIT-1.2.1 |
| MGIT-1.2.3 | Create Makefile | MGIT-1.2.1 |
| MGIT-1.2.4 | Create .golangci.yml | MGIT-1.2.1 |
| MGIT-1.2.5 | Create test infrastructure | MGIT-1.2.2 |

**Gate:** `make build` succeeds, `make lint` passes, test helpers compile.

### Wave 2: Domain Model Types (MGIT-2.1)

| Task | Title | Dependencies |
|------|-------|-------------|
| MGIT-2.1.1 | Implement Commit model | Wave 1 |
| MGIT-2.1.2 | Implement Branch model | Wave 1 |
| MGIT-2.1.3 | Implement TaskID model | Wave 1 |
| MGIT-2.1.4 | Implement FileDiff model | Wave 1 |
| MGIT-2.1.5 | Implement sentinel errors | Wave 1 |

**Gate:** All model types compile, validation methods pass, JSON serialization correct.

### Wave 3: go-git Repository Wrapper (MGIT-2.2)

| Task | Title | Dependencies |
|------|-------|-------------|
| MGIT-2.2.1 | Implement Repository wrapper | Wave 2 |
| MGIT-2.2.2 | Implement CommitStore | MGIT-2.2.1 |
| MGIT-2.2.3 | Implement TreeStore | MGIT-2.2.1 |
| MGIT-2.2.4 | Implement BranchStore | MGIT-2.2.1 |
| MGIT-2.2.5 | Implement DiffStore | MGIT-2.2.2, MGIT-2.2.3 |
| MGIT-2.2.6 | Implement WorktreeStore | MGIT-2.2.1 |
| MGIT-2.2.7 | Implement fsync-aware go-git storage wrapper | MGIT-2.2.1 |

**Gate:** All go-git operations pass against temp repos, no external git binary usage. Fsync wrapper verified with kill -9 crash test.

### Wave 4: SQLite Index + Store Integration (MGIT-2.3, MGIT-2.4)

| Task | Title | Dependencies |
|------|-------|-------------|
| MGIT-2.3.1 | Schema and migrations | Wave 2 |
| MGIT-2.3.2 | IndexStore open/close | MGIT-2.3.1 |
| MGIT-2.3.3 | task_commits operations | MGIT-2.3.2 |
| MGIT-2.3.4 | branches operations | MGIT-2.3.2 |
| MGIT-2.4.1 | go-git + SQLite integration tests | MGIT-2.3.3, Wave 3 |
| MGIT-2.4.2 | Append-only enforcement tests | MGIT-2.3.3 |
| MGIT-2.4.3 | Concurrent access tests | MGIT-2.3.3, MGIT-2.3.4 |

**Gate:** Store layer at 95% coverage, append-only enforced, concurrent access safe.

### Wave 5: Service Layer (MGIT-3.1, MGIT-3.2)

| Task | Title | Dependencies |
|------|-------|-------------|
| MGIT-3.1.1 | CommitService | Wave 4 |
| MGIT-3.1.2 | SquashService | MGIT-3.1.1 |
| MGIT-3.1.3 | RollbackService | MGIT-3.1.1 |
| MGIT-3.1.4 | BranchService | Wave 4 |
| MGIT-3.2.1 | DiffService | Wave 4 |
| MGIT-3.2.2 | VerifyService | MGIT-3.1.1 |
| MGIT-3.2.3 | AuditService | Wave 4 |
| MGIT-3.2.4 | ConfigService | Wave 4 |

**Gate:** Service layer at 95% coverage, squash atomicity verified, rollback append-only verified.

### Wave 6: Service Integration Tests (MGIT-3.3)

| Task | Title | Dependencies |
|------|-------|-------------|
| MGIT-3.3.1 | CommitService + SquashService integration | Wave 5 |
| MGIT-3.3.2 | RollbackService + VerifyService integration | Wave 5 |
| MGIT-3.3.3 | BranchService + CommitService integration | Wave 5 |
| MGIT-3.3.4 | Full workflow integration | Wave 5 |

**Gate:** All service combinations tested, no data corruption scenarios, error chains preserved.

### Wave 7: CLI Commands (MGIT-4.1, MGIT-4.2)

| Task | Title | Dependencies |
|------|-------|-------------|
| MGIT-4.1.1 | Root + init command | Wave 5 |
| MGIT-4.1.2 | commit command | MGIT-4.1.1, MGIT-3.1.1 |
| MGIT-4.1.3 | log command | MGIT-4.1.1, MGIT-3.1.1 |
| MGIT-4.1.4 | diff command | MGIT-4.1.1, MGIT-3.2.1 |
| MGIT-4.1.5 | status command | MGIT-4.1.1 |
| MGIT-4.1.6 | show command | MGIT-4.1.1, MGIT-3.1.1 |
| MGIT-4.1.7 | branch command | MGIT-4.1.1, MGIT-3.1.4 |
| MGIT-4.1.8 | config command | MGIT-4.1.1, MGIT-3.2.4 |
| MGIT-4.2.1 | rollback command | MGIT-4.1.1, MGIT-3.1.3 |
| MGIT-4.2.2 | squash command | MGIT-4.1.1, MGIT-3.1.2 |
| MGIT-4.2.3 | verify command | MGIT-4.1.1, MGIT-3.2.2 |
| MGIT-4.2.4 | export command | MGIT-4.1.1, MGIT-3.1.2 |
| MGIT-4.2.5 | audit command | MGIT-4.1.1, MGIT-3.2.3 |
| MGIT-4.2.6 | add command | MGIT-4.1.1 |
| MGIT-4.2.7 | cherry-pick command | MGIT-4.1.1, MGIT-3.1.1 |
| MGIT-4.2.8 | restore command | MGIT-4.1.1, MGIT-3.1.3 |
| MGIT-4.2.9 | checkout command | MGIT-4.1.1, MGIT-3.1.4 |
| MGIT-4.2.10 | merge command | MGIT-4.1.1, MGIT-3.1.4 |
| MGIT-4.2.11 | gc command | MGIT-4.1.1 |
| MGIT-4.2.12 | import command | MGIT-4.1.1 |

**Gate:** All 20 Wave 7 CLI commands (MGIT-4.1.1–4.1.8, MGIT-4.2.1–4.2.12) functional, help text present, error messages clear. Note: `serve`, `token`, and `docs generate` are implemented in Waves 8 and 10 respectively.

### Wave 8: REST API + MCP + mtix Integration (MGIT-5)

| Task | Title | Dependencies |
|------|-------|-------------|
| MGIT-5.1.1 | Router + middleware | Wave 5 |
| MGIT-5.1.2 | Commit endpoints | MGIT-5.1.1 |
| MGIT-5.1.3 | Branch + task endpoints | MGIT-5.1.1 |
| MGIT-5.1.4 | Squash + rollback endpoints | MGIT-5.1.1 |
| MGIT-5.1.5 | Token management command + auth middleware | MGIT-5.1.1 |
| MGIT-5.2.1 | MCP server setup | Wave 5 |
| MGIT-5.2.2 | Core MCP tools | MGIT-5.2.1 |
| MGIT-5.2.3 | Advanced MCP tools | MGIT-5.2.1 |
| MGIT-5.3.1 | mtix client wrapper | Wave 5 |
| MGIT-5.3.2 | Task-commit synchronization | MGIT-5.3.1 |
| MGIT-5.3.3 | Auto-squash on task completion | MGIT-5.3.2, MGIT-3.1.2 |

**Gate:** API endpoints respond correctly, MCP tools registered, mtix integration tested.

### Wave 9: E2E Tests + Performance (MGIT-6)

| Task | Title | Dependencies |
|------|-------|-------------|
| MGIT-6.1.1 | E2E commit lifecycle | Waves 7, 8 |
| MGIT-6.1.2 | E2E squash workflow | Waves 7, 8 |
| MGIT-6.1.3 | E2E rollback workflow | Waves 7, 8 |
| MGIT-6.1.4 | E2E branch lifecycle | Waves 7, 8 |
| MGIT-6.2.1 | Performance benchmarks (NFR-1) | Waves 7, 8 |
| MGIT-6.2.2 | Stress tests | Waves 7, 8 |
| MGIT-6.2.3 | Storage efficiency tests | Waves 7, 8 |

**Gate:** All E2E tests pass, NFR-1 performance targets met, stress tests stable.

### Wave 10: Agent Documentation (MGIT-7)

| Task | Title | Dependencies |
|------|-------|-------------|
| MGIT-7.1.1 | Docs generator framework | Wave 7 (Cobra commands exist) |
| MGIT-7.1.2 | Auto-generated section markers | MGIT-7.1.1 |
| MGIT-7.1.3 | CLI_REFERENCE.md auto-generation | MGIT-7.1.1, Wave 7 |
| MGIT-7.1.4 | MCP_TOOLS.md auto-generation | MGIT-7.1.1, Wave 8 (MCP tools exist) |
| MGIT-7.1.5 | SKILL.md auto-generation | MGIT-7.1.1 |
| MGIT-7.2.1 | AGENTS.md template | MGIT-7.1.1, MGIT-7.1.2 |
| MGIT-7.2.2 | CLAUDE.md template | MGIT-7.2.1 |
| MGIT-7.2.3 | WORKFLOWS.md template | MGIT-7.1.1 |
| MGIT-7.2.4 | ROLLBACK_GUIDE.md + SQUASH_GUIDE.md templates | MGIT-7.1.1 |
| MGIT-7.2.5 | TROUBLESHOOTING.md template | MGIT-7.1.1, MGIT-7.1.2 |
| MGIT-7.3.1 | mgit docs generate CLI command | MGIT-7.1.1, MGIT-7.2.* |
| MGIT-7.3.2 | mgit_docs_generate MCP tool | MGIT-7.3.1, Wave 8 |
| MGIT-7.3.3 | Integrate with mgit init (FR-15.4) | MGIT-7.3.1 |
| MGIT-7.2.6 | Requirements Traceability Matrix generator | MGIT-7.1.1, MGIT-7.1.2 |

**Gate:** `mgit docs generate` produces all 9 files plus RTM. SKILL.md has valid YAML frontmatter. CLI_REFERENCE.md matches actual commands. MCP_TOOLS.md matches actual tools. RTM maps all FR/NFR to design, code, tests. Auto-section markers preserve human edits.

### Wave 11: Agent Worktrees (MGIT-8)

| Task | Title | Dependencies |
|------|-------|-------------|
| MGIT-8.1.1 | WorktreeInfo model + WorktreeManager interface | MGIT-2.1.3 |
| MGIT-8.1.2 | Worktrees table in SQLite index | MGIT-2.3.1 |
| MGIT-8.1.3 | v1 WorktreeManager (filesystem-backed) | MGIT-8.1.1, MGIT-8.1.2, MGIT-2.2.6 |
| MGIT-8.2.1 | WorktreeService | MGIT-8.1.3, MGIT-3.1.1, MGIT-3.1.4 |
| MGIT-8.2.2 | Worktree integration tests | MGIT-8.2.1 |
| MGIT-8.3.1 | mgit worktree CLI commands | MGIT-8.2.1, MGIT-4.1.1 |
| MGIT-8.3.2 | Worktree MCP tools | MGIT-8.2.1, MGIT-5.2.1 |
| MGIT-8.3.3 | Update docs generator for worktree commands | MGIT-8.3.1, MGIT-8.3.2, MGIT-7.1.3, MGIT-7.1.4 |

**Gate:** `mgit worktree add/list/remove/prune` functional. MCP tools registered. Two concurrent worktrees can commit to different tasks without interference. Task binding enforced. Stale worktree pruning works. Docs generator includes worktree commands and tools. Pluggable interface verified: WorktreeManager can be swapped without CLI/service changes (see ADR-004).

---

## 3. Critical Path

The minimum path to a working mgit binary:

```
MGIT-1.2 → MGIT-2.1 → MGIT-2.2 → MGIT-2.3 → MGIT-2.4 → MGIT-3.1 → MGIT-3.2 → MGIT-4.1 → MGIT-5.1 → MGIT-6.1 → MGIT-7.1
```

Parallel tracks (after Wave 4):
- **Track A (Services):** MGIT-3.1 → MGIT-3.2 → MGIT-3.3
- **Track B (CLI):** MGIT-4.1 → MGIT-4.2 (depends on Track A)
- **Track C (API/MCP):** MGIT-5.1 → MGIT-5.2 → MGIT-5.3 (depends on Track A)
- **Track D (Agent Docs):** MGIT-7.1 → MGIT-7.2 → MGIT-7.3 (depends on Tracks B + C)
- **Track E (Worktrees):** MGIT-8.1 → MGIT-8.2 → MGIT-8.3 (depends on Track A + B + C + D)

---

## 4. Task Pickup Protocol (via mtix MCP)

Tasks are managed by **mtix** (micro-tix). Use mtix MCP tools for all task operations.

1. Call `mtix_ready` to find unblocked, unclaimed tasks on the current wave
2. Call `mtix_context` with the task ID — this returns the full ancestor chain:
   - **Story prompt**: project-wide architectural context
   - **Epic prompt**: feature-area scope, interfaces, patterns
   - **Issue prompt**: exact implementation spec + acceptance criteria
3. Call `mtix_claim` with your agent ID to claim the task
4. Read referenced FR/NFR sections in REQUIREMENTS.md (cited in the assembled prompt)
5. Follow the TDD cycle from TDD-WORKFLOW.md
6. On completion: run verification checklist from CLAUDE.md, then call `mtix_done`
7. If blocked: use `mtix_dep_add` to declare the blocking dependency (blocked status is auto-managed), then pick next task from `mtix_ready`

---

## 5. Verification Gates

Before advancing to the next wave, verify ALL of the following:

```bash
# All tests pass
go test ./... -count=1

# Race detector clean
go test ./... -race -count=1

# Coverage meets threshold
go test ./... -coverprofile=cover.out -count=1
go tool cover -func=cover.out | tail -1
# Store/Service: ≥95% | Model/CLI/API/MCP: ≥90% | Overall: ≥90%

# Linter clean
golangci-lint run

# Vulnerability scan clean
govulncheck ./...

# Build succeeds
go build -o mgit ./cmd/mgit/
```

All six checks MUST pass. If any fail, fix before proceeding.

---

## 6. Task Statistics

| Metric | Value |
|--------|-------|
| Total stories | 8 |
| Total epics | 22 |
| Total issues | 102 |
| Total nodes | 132 |
| Done (story + epic + issues) | 8 |
| Open | 124 |
| Waves | 11 |
| Estimated test count | ~420 |

---

## 7. Document Cross-References

| Document | Purpose | Read When |
|----------|---------|-----------|
| CLAUDE.md | Agent directives, forbidden patterns | First session |
| REQUIREMENTS.md | FR/NFR specifications | Every task (referenced sections) |
| CODING-STYLE.md | Architecture, naming, go-git patterns | First session |
| QUALITY-STANDARDS.md | Coverage targets, static analysis | First session |
| TDD-WORKFLOW.md | Test-driven process | Every task |
| APPROVED-PACKAGES.md | Allowed dependencies | When adding imports |
| PACKAGE-APPROVAL-PROCESS.md | New dependency process | If you need an unapproved package |
| ADR-001-EMBEDDED-GIT.md | Why go-git was chosen | For context on git engine decisions |
| ADR-002-DUAL-HASH-MODEL.md | SHA-1 (go-git) + SHA-256 (mgit) strategy | FR-2, FR-3, FR-12, NFR-5 tasks |
| ADR-003-DO178C-SCOPE.md | DO-178C applicability scope | Safety-critical context, Wave 5 services |
| ADR-004-PLUGGABLE-WORKTREE.md | Pluggable worktree strategy (v1 mgit-managed, v2 go-git v6) | Wave 11 worktree tasks |
| requirement-branch-strategy.md | Branch model details | FR-5 tasks |
| requirement-squash-algorithm.md | Squash algorithm details | FR-7 tasks |
| requirement-rollback-semantics.md | Rollback behavior details | FR-6 tasks |
| mtix MCP tools | Task specs, context assembly, status management | Every task (use mtix_context, mtix_ready, mtix_claim, mtix_done) |

---

*The documentation is the first system we build. If it has bugs, the software will too. This documentation suite is verified and ready for development.*
