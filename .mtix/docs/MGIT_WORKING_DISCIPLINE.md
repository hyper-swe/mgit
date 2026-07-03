# mgit Working Discipline — Agent Skill

> The step-by-step discipline for working a task with **mgit**, the safe,
> checkpointed working substrate for HyperSwe coding agents. Every command
> below exists in the shipped CLI; verify any flag with `mgit <cmd> --help`.

mgit is git underneath (go-git). Its value is not novel storage — it is the
agent workflow plus the sandbox-to-land integration. You micro-commit each
coherent step into an isolated `.mgit` store that never touches the project's
real `.git`, course-correct cheaply when a decision turns out wrong, and land
only the squashed, reviewed result.

> **mgit keeps itself in sync with git — there is no manual `mgit sync` step**
> (ADR-008). git is authoritative; mgit automatically keeps its `.mgit` base
> coherent with your current local working state. A new task worktree therefore
> carries your unpushed local foundation, and each task pins the base it forked
> from, so a later resync never corrupts its diff. mgit reads `.git` read-only
> to learn git's state and never mutates it. You never run a resync by hand; if
> mgit cannot safely read git state it fails loud rather than materialize a
> stale worktree.

This skill is written for **both**:
- an **autonomous agent** working a task end to end, and
- a **reviewer** directing course-correction on an agent's work.

Course-correction (backtrack / fork / cherry-pick) is **cheap and supported**,
and this skill instructs you to use it — but note it is not yet validated that
agents reliably reach for it on their own (MGIT-28 is the pending head-to-head
test). The most reliable actor directing it today is a reviewer.

---

## 1. Pick up the task

Get your task context from mtix (assembled context chain → your full
briefing), then claim it. Do not write code without a task.

```bash
mtix ready                      # find unblocked, unclaimed work
mtix context MGIT-12.3          # MANDATORY: read the assembled briefing
mtix claim  MGIT-12.3 --agent <agent-id>
```

## 2. Start the agent on the task with `mgit work`

`mgit work` is the first-class "start an agent on a task" entry point. It
provisions a task-bound mgit worktree (FR-16) and wires the agent's shell to
route through `mgit run` into the task sandbox (CLAUDE.md + .claude/settings.json).

```bash
# Worktree + agent wiring only (sandbox launched later / on another host):
mgit work ./wt-MGIT-12.3 --task MGIT-12.3

# Also launch the microVM sandbox now (requires a digest-pinned image):
mgit work ./wt-MGIT-12.3 --task MGIT-12.3 \
  --sandbox --image base@sha256:<hex> --network allowlist --allow registry.npmjs.org
```

If the sandbox backend is unavailable the worktree and wiring still succeed;
only the sandbox leg fails closed (NFR-17.6). All work below runs **inside**
that worktree.

> Prefer `mgit work` over raw `mgit worktree add` — `work` also does the agent
> wiring. Use `mgit worktree add ./path --task ID` only when you want the
> worktree without the agent-shell integration.

> **Build artifacts in a worktree.** A worktree is seeded from `.mgit` honoring
> `.gitignore`, so gitignored **generated** artifacts (e.g. an embedded
> `web/dist`) are NOT carried in — a fresh worktree may fail `go build ./...`
> until they exist, exactly like a fresh git checkout. Either run the
> generate/build step in the worktree, or list the build-required paths in
> `.mgit/seed-include` (one glob per line, e.g. `web/dist`) to carry them from
> the working tree into every new worktree. The task-id flag is `--task-id`
> everywhere (`--task` is accepted as a back-compat alias).

## 3. Micro-commit every coherent step

Commit as soon as a step compiles or passes — micro-commits are cheap and
expected; they are collapsed at land, so do not batch or hesitate. Inside a
bound worktree the task ID is auto-inherited.

```bash
mgit status                              # what's changed
mgit add .                               # stage
mgit commit -m "add validation helper"   # task ID auto-inherited in the worktree
# Outside a bound worktree, pass it explicitly:
mgit commit --task-id MGIT-12.3 -m "add validation helper"
```

`mgit commit` takes `--task-id` (NOT `--task`). When a sandbox is active for the
worktree, `mgit run -- <cmd>` routes builds/tests/installs into the microVM and
your wired shell does this transparently. When no sandbox is active (none was
requested, or the daemon is not installed), the worktree's `CLAUDE.md` says so
plainly and you run commands normally on the host — there is no routing to
worry about.

## 4. Orient between steps

```bash
mgit log --oneline               # the steps you've taken so far
mgit log --task-id MGIT-12.3     # just this task's commits
mgit diff                        # uncommitted changes
mgit diff --task-id MGIT-12.3    # cumulative diff for the task
mgit show <hash>                 # one commit in detail
```

## 5. Course-correct — backtrack, fork, salvage (don't restart)

When a prior decision proves wrong (e.g. a bad library choice), return to the
decision point instead of rewriting hundreds of lines from scratch.

**Backtrack** — append-only revert of a task or a specific commit (the reverted
attempt stays in history for review):

```bash
mgit rollback --task-id MGIT-12.3 --reason "wrong validation lib"
mgit rollback --commit <hash> --reason "revert just this step"   # resolves task automatically
```

**Fork** — branch a new line from a good commit and continue the new approach,
preserving the old line:

```bash
mgit checkout <good-hash>          # move to the decision point
mgit checkout -b task/MGIT-12.3-v2 # fork a new branch from here
```

**Salvage** — cherry-pick the still-good work from the old line onto the new one:

```bash
mgit cherry-pick <useful-hash>                       # apply onto current branch
mgit cherry-pick <useful-hash> --onto task/MGIT-12.3-v2
mgit cherry-pick <useful-hash> --no-commit           # preview only
```

A reviewer can drive exactly these steps after inspecting `mgit log`/`mgit diff`.

## 6. Hand off for review

The append-only history is the review surface: every step, including reverted
attempts, is visible. The reviewer reads `mgit log --task-id <ID>` and
`mgit diff --task-id <ID>` and can direct any of the step-5 course-corrections
before the work lands.

```bash
mgit log --task-id MGIT-12.3 --format full
mgit diff --task-id MGIT-12.3
mgit verify --task-id MGIT-12.3   # commit chain + index integrity
```

## 7. Squash and land

Squash collapses the task's micro-commits into one reviewable commit. Land
exports/promotes only that result.

```bash
mgit squash --task-id MGIT-12.3 --dry-run            # preview
mgit squash --task-id MGIT-12.3                      # squash in place
mgit squash --task-id MGIT-12.3 --to-git --to-git-output task.patch  # export as git format-patch
mgit squash --task-id MGIT-12.3 --to-main            # fast-forward the squash onto main
```

**Integrate by APPLYING the squash patch — never by hand-diffing files.** An mgit
worktree is not a git repo and git stays canonical, so the `--to-git` patch IS the
bridge: apply it to the project's git with `git apply task.patch` (or
`git am < task.patch`). Do NOT reconstruct the change by manually diffing files
between trees — that loses git semantics and is error-prone. The patch is a real
`diff --git` with content and round-trips cleanly (MGIT-33).

If the task ran in a sandbox, land the verified changes through the airlock:

```bash
mgit sandbox land --task MGIT-12.3   # pull + host-verify (dual-hash + task binding) + append
```

Then mark the task done in mtix once acceptance criteria and tests pass:

```bash
mtix done MGIT-12.3
```

---

## Command quick-reference

| Step | Command |
|------|---------|
| Start a task | `mgit work <path> --task <ID>` |
| Commit a step | `mgit commit -m "..."` (task auto-inherited) / `--task-id <ID>` |
| Run build/test/install | `mgit run -- <command>` |
| Orient | `mgit status` · `mgit log --oneline` · `mgit diff [--task-id <ID>]` · `mgit show <hash>` |
| Backtrack | `mgit rollback --task-id <ID>` / `--commit <hash>` |
| Fork | `mgit checkout <hash>` then `mgit checkout -b <branch>` |
| Salvage | `mgit cherry-pick <hash> [--onto <branch>]` |
| Verify | `mgit verify --task-id <ID>` |
| Squash | `mgit squash --task-id <ID> [--to-git \| --to-main]` |
| Land (sandbox) | `mgit sandbox land --task <ID>` |

## Common pitfalls (and the fix)

Friction real agents have hit working through mgit — know these up front so you
don't rediscover them by stumbling:

1. **A worktree is NOT a git repo.** There is no `.git` inside it, so `git
   status` / `git diff` / `git commit` / `git apply` do not work there and
   git-trained habits mislead you. Use the mgit equivalents (`mgit status`,
   `mgit diff`, `mgit commit`), and integrate by exporting the squash:
   `mgit squash --task-id <ID> --to-git | git apply` — run the `git apply` from
   the **project root**, never inside the worktree.

2. **Gitignored build artifacts are not seeded.** A worktree is seeded from
   `.mgit` honoring `.gitignore`, so generated/embedded artifacts (e.g. a
   `//go:embed`-ed `web/dist`) are absent and a full build can fail — exactly
   like a fresh `git checkout`. Fix: run the generate/build step in the
   worktree, or list the build-required paths in `.mgit/seed-include` (one glob
   per line) to carry them in.

3. **Do not manually re-import to "refresh" a worktree.** mgit auto-housekeeps
   its base from your current local working state, so you never need
   `mgit add . && mgit commit` to pick up just-integrated work. If a worktree
   looks stale, that is a bug to report — not a manual sync step to run.

4. **The task-id flag is `--task-id`** on every command (`--task` is accepted as
   an alias). Don't guess — prefer `--task-id`.

5. **Task IDs accept dotted and dashed forms** — `MGIT-1.2.3`, `MTIX-30.6`,
   `MTIX-30-probe` are all valid. Genuinely unsafe ids (path separators,
   whitespace, shell/SQL metacharacters) are rejected with an error that *names*
   the accepted grammar — read it instead of guessing.

6. **mgit does not make parallelism safe by itself — disjoint scope does.**
   Running N agents in N worktrees prevents store collisions, but two agents
   editing the same files still conflict at land. Give each parallel agent a
   non-overlapping set of files/areas; mgit adds containment + a task-tagged
   audit trail + cheap checkpointing *on top of* that discipline, not instead of
   it.

7. **The sandbox fails closed.** When a sandbox is wired, `mgit run` routes
   execution into the microVM and does **not** fall back to the host if the
   backend is unavailable. A blocked command is a policy/availability signal
   (often `MGIT-EGRESS-DENIED …` with a `remedy=`), not something to retry on
   the host.

8. **Check `mgit version` if behaviour seems off.** It reports the real
   version/commit/date; a surprising result is usually a stale binary, not an
   mgit bug.

9. **Know when mgit is the right tool.** It earns its keep when you can't/won't
   push WIP (a worktree carries your *unpushed* local foundation), you want the
   task&rarr;commit audit trail + the mtix loop, or you need the microVM sandbox
   for untrusted code. If you push WIP freely and the code is trusted, a plain
   `git worktree` is lighter — use it instead.

10. **A missing sandbox does not make mgit unusable.** On a machine without
    `mgit-sandboxd`, `mgit work` still gives you a real, usable worktree — it
    just does not install the fail-closed routing, and the `CLAUDE.md` block
    states "no sandbox on this host; commands run on the host". Build, test, and
    `mgit commit` normally. `mgit work` prints one parseable line —
    `Containment: none|requested|active` — so you can tell the posture at a
    glance. Only `mgit run` (which fails closed with an install pointer) and
    `mgit sandbox land` require the daemon.

11. **Landing without a sandbox.** `mgit sandbox land` is sandbox-only (it
    host-verifies the guest's attested changes). With no sandbox, land a task by
    exporting its squash and applying it to your git:
    `mgit squash --task-id <ID> --to-git | git apply` (or `git am`), from the
    project root. See [docs/INSTALL-SANDBOX.md](../../docs/INSTALL-SANDBOX.md) to
    enable the sandbox.

12. **"another mgit process is running" means a server holds the lock.** A
    running `mgit serve` / MCP server used to hold the repo lock for its whole
    lifetime and starve the CLI; it now takes the lock only per operation, so
    this error is brief and self-clears. If it persists, a stuck command or
    server is holding it — the error names the holding command, not just a PID.

13. **The MCP worktree tools are real.** `mgit_worktree_add` / `list` / `remove`
    (and every other MCP tool) delegate to the same service layer as the CLI and
    return real results — they are no longer placeholders. `mgit_worktree_add`
    materializes a real task-bound worktree. Agent input is validated as
    hostile, so a malformed task id or a traversal path comes back as a
    structured tool error.

## Honest caveats

- mgit is git underneath; the moat is the agent UX/discipline + the
  sandbox-land integration, not the storage. The honest comparison is
  "git + a scratch-branch convention."
- The backtrack/fork/cherry-pick loop is **cheap and instructed here** but not
  yet validated as something agents reliably self-initiate (MGIT-28). Reviewers
  are the most reliable directors of course-correction today.
- macOS containment defaults to a vzf + **Linux** guest; a mac-native profile
  for Swift/Xcode/brew workloads is a planned opt-in (MGIT-27). There is no
  seamless macOS-native execution today.
