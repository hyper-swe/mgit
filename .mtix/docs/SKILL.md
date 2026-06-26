---
description: "Manage tasks in MGIT project using mtix micro-issue manager. Use for creating, decomposing, claiming, completing tasks, and assembling context chains for agent briefings."
allowed-tools:
  - mcp__mtix__mtix_create
  - mcp__mtix__mtix_show
  - mcp__mtix__mtix_list
  - mcp__mtix__mtix_done
  - mcp__mtix__mtix_claim
  - mcp__mtix__mtix_unclaim
  - mcp__mtix__mtix_context
  - mcp__mtix__mtix_decompose
  - mcp__mtix__mtix_search
  - mcp__mtix__mtix_ready
  - mcp__mtix__mtix_tree
  - mcp__mtix__mtix_update
  - mcp__mtix__mtix_defer
  - mcp__mtix__mtix_cancel
  - mcp__mtix__mtix_reopen
  - mcp__mtix__mtix_comment
  - mcp__mtix__mtix_dep_add
  - mcp__mtix__mtix_dep_remove
  - mcp__mtix__mtix_dep_list
  - mcp__mtix__mtix_session_start
  - mcp__mtix__mtix_session_end
  - mcp__mtix__mtix_agent_register
  - mcp__mtix__mtix_agent_heartbeat
  - mcp__mtix__mtix_agent_state
  - mcp__mtix__mtix_agent_work
  - mcp__mtix__mtix_stats
  - mcp__mtix__mtix_progress
  - mcp__mtix__mtix_stale
  - mcp__mtix__mtix_blocked
  - mcp__mtix__mtix_verify
  - mcp__mtix__mtix_export
  - mcp__mtix__mtix_import
  - mcp__mtix__mtix_backup
  - mcp__mtix__mtix_gc
  - mcp__mtix__mtix_config
---

# MGIT — mtix Skill

This skill enables AI agents to work on the **MGIT** project using mtix task management. mgit is the safe, checkpointed working substrate for HyperSwe coding agents: an agent works each task inside an isolated, version-controlled worktree, micro-commits every coherent step, and lands only the reviewed result. The engineering discipline below (context-chain traversal, no-code-without-a-task, no-stub completeness) is the operating standard — high-integrity because the substrate is load-bearing, not because of any compliance regime.

> **Working a task with mgit.** Start an agent on a task with `mgit work <path> --task <ID>` — it provisions a task-bound mgit worktree and wires the agent's shell to route through `mgit run` (the sandbox airlock). Inside the worktree, `mgit commit -m "..."` records each step (task ID auto-inherited); `mgit log` / `mgit status` / `mgit diff` orient you; `mgit rollback` / `mgit checkout -b` / `mgit cherry-pick` course-correct without restarting; `mgit squash --task-id <ID> --to-git` collapses the work for the land. See [the mgit agent skill](MGIT_WORKING_DISCIPLINE.md) for the full command-level discipline.

## NON-NEGOTIABLE: Context Chain Traversal

**Before doing ANY work, call `mcp__mtix__mtix_context` with the node ID.** The assembled prompt from root→node IS your complete briefing — business goal, technical scope, exact instructions, acceptance criteria, and test specifications.

- Do NOT work from titles alone
- Do NOT skip this step under any circumstances
- If the assembled context is insufficient to execute independently, STOP and escalate — the parent task decomposition is incomplete

The dot-notation task hierarchy (e.g., `MGIT-1.3.2`) is a context chain:
- **Root** → business goal and constraints
- **Middle levels** → technical approach and scope
- **Leaf** → exact implementation instructions (files, functions, tests)

## NON-NEGOTIABLE: No Code Without a Task

**NEVER write code without a corresponding mtix task.** If no task exists, create one first with `mcp__mtix__mtix_create` — every task must have `description`, `prompt`, and `acceptance` populated with enough detail that a different agent can execute it independently using only the assembled context chain. Apply the completeness test: *"Can an agent with zero conversation history execute this task from the assembled prompt alone?"* This applies to features, bug fixes, refactors, and even one-line changes.

## Execution Workflow

1. **Start session:** `mcp__mtix__mtix_session_start`
2. **Find work:** `mcp__mtix__mtix_ready`
3. **Read context:** `mcp__mtix__mtix_context` — **MANDATORY before every task**
4. **Claim:** `mcp__mtix__mtix_claim`
5. **Send heartbeats** every 5 minutes: `mcp__mtix__mtix_agent_heartbeat`
6. **Execute** following the assembled prompt
7. **Verify** all acceptance criteria before proceeding
8. **Mark done:** `mcp__mtix__mtix_done`
9. **End session:** `mcp__mtix__mtix_session_end`

## Before Marking Done

- All acceptance criteria explicitly verified
- Tests written and passing — no stub implementations
- Independent verification: implementing agent ≠ verifying agent for critical tasks
- Traceability comment added via `mcp__mtix__mtix_comment` linking task→requirement→test→result
- No functions with "not yet implemented" or placeholder logic

## Decomposition Rules

When creating or decomposing tasks, every leaf node MUST have:
- **description** — scope, constraints, why this task exists
- **prompt** — file paths, function names, API contracts, edge cases, test scenarios
- **acceptance** — testable criteria that define "done"
- **tests** — test function names and scenarios

The completeness test: *"Can an agent execute this task using ONLY the assembled context from root to this node?"*

## Specialized Skills

For deeper guidance, `mtix plugin install` provides role-specific skills:
- **mtix-task-execution.md** — detailed execution protocol with error recovery
- **mtix-planning.md** — decomposition rules and context chain writing guide
- **mtix-review.md** — audit, verification, and progress tracking
- **mtix-multi-agent.md** — agent coordination and handoff protocols
- **mtix-admin.md** — backup, export/import, garbage collection

## Reference Documentation

- [MGIT_WORKING_DISCIPLINE.md](MGIT_WORKING_DISCIPLINE.md) — the mgit working substrate: micro-commit → backtrack → fork → cherry-pick → squash → land
- [AGENTS.md](AGENTS.md) — full agent operating guide
- [CONTEXT_CHAIN.md](CONTEXT_CHAIN.md) — writing effective task descriptions
- [STATUS_MACHINE.md](STATUS_MACHINE.md) — state transitions
- [WORKFLOWS.md](WORKFLOWS.md) — decompose-claim-done patterns
- [TROUBLESHOOTING.md](TROUBLESHOOTING.md) — common errors and solutions

## Engineering Discipline

mgit is held to a high-integrity engineering bar — not a compliance regime, but the standard that keeps the substrate trustworthy:
- **Test-driven** — no production code without a failing test first; coverage bars enforced.
- **Append-only audit** — commits are never deleted; rollbacks create revert commits, preserving every attempt for review.
- **Parameterized SQL + go-git determinism** — no string-built queries, no shelling out to git.
- **No stubs** — a task is done only when every acceptance criterion is real, working code with passing tests.

OWASP ASVS Level 2 informs the security mindset. See the root `CLAUDE.md` for the full contributor discipline.
