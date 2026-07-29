# Feature → e2e coverage matrix

What each mgit capability is proven by, end to end. "e2e" means a real
installed-style binary (or real server process) driven by `scripts/e2e/*` —
the jobs in `.github/workflows/e2e.yml`, which gate every release
(`release.yml` `needs: e2e`) and run locally via `make e2e` — **or** a Go test
that itself compiles and drives the real `mgit` binary as a subprocess
(verified below; several such tests exist outside `scripts/e2e/*` and were
previously uncredited here). "unit" means covered by `go test` (90/85
coverage bar) but never touching a real binary/process, even when a test is
*named* `TestE2E_*`. Update this file whenever an e2e job or script changes
(MGIT-53); a claim in the README with no e2e row here is a gap, not an
oversight to paper over.

Legend: **e2e** proven end-to-end (real binary/process) · **gated** e2e
exists but needs environment (skips in hosted CI) · **unit** unit/integration
tests only, including anything self-labeled `TestE2E_*` that never spawns a
binary · **—** uncovered (no test of any kind touches the claim, or the claim
isn't tested at the granularity implied).

This revision (2026-07-29) was rebuilt from a full audit of every CLI
command/flag, every REST route, every MCP tool, every `scripts/e2e/*` file,
and every `TestE2E_*`/e2e-named Go test in the repo — not just what the
previous version already listed. **Finding gaps was the point of the
exercise; the "—" and flagged rows below are the most important content in
this file, not an afterthought.**

## Core loop, course-correction, install posture

| Capability / claim | Proof | Where |
|---|---|---|
| `init` over a **fresh** repo, worktree add/list/remove, task-inherited `commit`, `add`/`status`/`log`/`verify`/`audit`, `squash --to-git \| git apply` round-trip | e2e | core_loop.sh |
| `init` over an **existing** git repo with real prior history — byte-for-byte `.git` untouched before/after | e2e | `e2e/coexist_e2e_test.go::TestE2E_FullLifecycle_OverRealGitRepo_HistoryIntact` (spawns the real compiled binary; **not previously listed in this file** — `core_loop.sh`'s `init` is a fresh scratch repo and never checks this claim at all) |
| **Course-correction loop**: 3 micro-commits (one wrong) → content-restoring `rollback` (files removed from disk, not just metadata) → `checkout -b` fork → checkpoint-bounded `restore --all --commit` → materializing `cherry-pick` with provenance → `squash --to-git` (corrected content only) — append-only survives every step | e2e | course_correction.sh |
| `squash --to-main`: promotes a task branch onto `main` via real merge, originals untouched | e2e | `e2e/squash_semantics_e2e_test.go::TestE2E_Squash_LandsOnTaskBranch_MainUntouched` (real binary; corrects an earlier revision of this file, which mischaracterized this as zero-coverage) |
| `squash --to-main`'s worktree guard (rejects `--to-main` from a linked worktree, naming the bound task) | unit | `cmd/mgit/squash_to_main_test.go` — previously the guard itself (as opposed to the promote-to-main behavior above) had no cmd-level test; added 2026-07-29 |
| `squash --apply` (documented as a `--to-git` alias) | **—** | never invoked anywhere; likely-defective flag interaction (sets `toGit=true` with no output path) is unverified |
| `cherry-pick --onto <branch>` / `--no-commit`, incl. the worktree guard rejecting `--onto` from a linked worktree | **—** | bare `cherry-pick <hash>` is e2e'd (course_correction.sh); these two flag variants have zero coverage at any level |
| `rollback --to-commit` (alias) / `--dry-run` / task-id-driven (vs. positional hash) | **—** | only the positional-hash + `--reason` form is e2e'd |
| `restore <file> <commit>` (per-file form) / `--force` | **—** | only `restore --all --commit` (whole-tree) is e2e'd, and without `--force` |
| `checkout <existing-branch>` (plain switch, non-`-b`) | unit | only `checkout -b` is e2e'd (course_correction.sh); plain switch covered by checkout_service_test.go only |
| Multi-agent, **real OS-process** concurrency: 10 real `mgit` processes racing the same repo (different tasks / same task) and 10 processes across separate repos | e2e | `e2e/concurrent_cli_test.go` (`TestConcurrent_10Agents_SameRepo_DifferentTasks`, `TestConcurrent_10Agents_SeparateRepos`, `TestConcurrent_SameTask_Race`) — spawns real binaries via `exec.Command`; **stronger proof than the row below and not previously cited anywhere** |
| Multi-agent parallel worktrees (N tasks side by side), in-process | unit | `internal/service` worktree tests (`TestWorktreeIntegration_ConcurrentWorktrees` is in-process and not actually concurrent despite its name — see the row above for the real proof) |
| Two worktrees on disjoint tasks: commit isolation, parent `main` untouched, and hard guards (`checkout main`/`branch main`/`squash --to-main`/`cherry-pick --onto main`/foreign-task `rollback` all refuse from a worktree) | e2e | `e2e/worktree_commit_e2e_test.go` (real binary; not previously listed) |
| `mgit` run from a nested subdirectory resolves `.mgit` by walking up | e2e | `e2e/subdir_cli_test.go::TestE2E_RunsFromSubdirectory` (real binary; not previously listed) |
| `rollback` by bare commit hash completes promptly (regression guard vs. double-locking) | e2e | `e2e/rollback_cli_test.go::TestE2E_RollbackByCommitHash_CompletesPromptly` (real binary; not previously listed) |
| ADR-008 auto-resync / pinned fork-base | unit | `internal/service` sync tests |
| Install channels produce working binaries (`go install`, release archive incl. `mgit-sandboxd`) | e2e | e2e.yml install-channels matrix |
| Daemon-less honest posture (`mgit work` open, no shims, truthful CLAUDE.md, `Containment:` line) | e2e | daemonless_posture.sh |
| `mgit run` fails closed with install pointer when no daemon | e2e | daemonless_posture.sh |

### Misnamed "unit" tests — self-labeled `TestE2E_*` but never touch a binary

`e2e/lifecycle_test.go` (`TestE2E_CommitLifecycle`, `TestE2E_SquashWorkflow`,
`TestE2E_RollbackWorkflow`, `TestE2E_BranchLifecycle`) and
`e2e/bench_test.go::TestE2E_StorageEfficiency` only wire
`service.NewCommitService(...)` etc. directly in-process
(`setupServiceEnv`) — by this file's own legend they are **unit**, not e2e,
regardless of their name. Not a coverage gap (the service layer is exercised
elsewhere too), but a naming/quality-signal mismatch worth fixing so the name
doesn't overstate the proof.

## REST (10 routes) and MCP (15 tools)

Every REST route and every MCP tool has (a) a handler-level unit test, (b) a
literal invocation in the corresponding e2e harness, and (c) a name in
`docs/MCP-PARITY.md`'s capability table — confirmed route-by-route and
tool-by-tool, not just at the "REST posture" / "MCP posture" summary level
below. **No orphaned route or tool was found** (nothing exists in code that
both docs are silent on) and no stale/optimistic doc claim was found either
(every MCP-PARITY.md claim checked out against real handler code and a real
e2e call site).

| Capability / claim | Proof | Where |
|---|---|---|
| REST: all 10 documented `/api/v1` routes (`GET /health`, commits create/get/list, task commits, branches create/list, squash, rollback, verify) over a real `mgit serve` process, incl. 404/400 error shaping | e2e | rest_posture.sh; per-route detail in [MCP-PARITY.md](MCP-PARITY.md)'s REST-scope section |
| REST: loopback-only bind | e2e (partial) | rest_posture.sh asserts the serve log announces `127.0.0.1`; the non-loopback-unreachable check is best-effort and **skips itself** when the runner has no non-loopback interface (typical hosted CI) |
| REST: unauthenticated same-user model | e2e (implicit) + unit | rest_posture.sh never sends an `Authorization` header and every call still succeeds, but there is no explicit assertion targeting the auth posture itself; serve tests cover the rest |
| REST: serve/CLI per-operation lock coexistence (MGIT-46) | e2e | rest_posture.sh (bounded-timeout CLI ops alongside a live `serve`) |
| MCP: all 15 registered tools present, no placeholder text | e2e | mcpdrive (`tools/list` assertion) |
| MCP: every tool driven through real stdio, incl. cross-tool consistency (commit → log/show/diff agree) | e2e | mcpdrive |
| MCP: hostile input on every tool → structured tool errors, not crashes | e2e | mcpdrive (`driveHostileInputs`) |

## Sandbox (FR-17)

| Capability / claim | Proof | Where |
|---|---|---|
| Sandbox: launch → `run` in guest → verified `land` (firecracker/vzf) | gated | sandbox_posture.sh (needs daemon + KVM/entitlement + guest image; live pass per platform mandated by docs/release/RELEASE-CHECKLIST.md). **Its `land` assertion is exit-code only** (`assert_ok`) despite the script's own comment claiming it "verifies dual-hash + task binding + host-anchored attestation" — no field-level check exists in this script; see the row below for where that's actually proven |
| Firecracker (Linux GA default): exec round-trip, land round-trip, notify auto-land, overlay-root writability, SEC-03 hostile-guest (shared-store unreachable, escaping-symlink rejected, land-is-only-bridge) | gated | `internal/sandboxd/backend/firecracker`'s `TestE2E_*` (env-gated on `/dev/kvm` + `MGIT_TEST_KERNEL`/`MGIT_E2E_GUEST_ROOTFS`); re-validated live on the Linux runner, most recently 2026-07-30 against the go-git/go-billy upgrade and the SEC-09 fix below — **11 of 15 tests pass**, 4 skip (root-gated: allowlist/open network modes, both port-publish variants) — exec/land roundtrip, hostile-guest ×3, notify, overlay, provenance, claim-to-land, remove-discard, none-mode network. (An earlier revision of this file said "12/15" — a miscount corrected here; the pass/skip set itself has been identical and reproducible across every re-run this branch.) |
| Firecracker: allowlist/open network modes, port publish (host→guest reachable, guest→host-loopback denied) | gated, root-required | same package, `TestE2E_Network_Allowlist_ProxyAndDNSEnforced`, `TestE2E_Network_Open_NATEgress`, `TestE2E_PortPublish_*` — additionally need root for tap+iptables; **pending live re-run under sudo on the Linux runner as of this revision** |
| vzf (macOS GA default): exec/land round-trip, full claim→land→remove lifecycle with per-commit provenance, SEC-03 hostile-guest (same three claims as firecracker) | gated | `internal/sandboxd/backend/vzf`'s `TestE2E_VZF_*` (env-gated on `MGIT_E2E_VZF_KERNEL`+`MGIT_E2E_GUEST_ROOTFS`); **this is the file/test-level detail the previous "backend e2e (env-gated)" row gestured at without naming** |
| libkrun (cross-platform, opt-in on Linux via `-tags libkrun`, default on macOS): build+link, unit suite (~30 tests incl. egress litmus, SEC-03 staging, env-leak check) | gated | `internal/sandboxd/backend/libkrun`'s `go test -tags libkrun` suite; passes on both macOS/HVF and, as of the 2026-07-29 Linux investigation, on real Linux/KVM too |
| libkrun: real-VM boot to a completing guest workload on Linux/KVM | **—** | `TestE2E_Libkrun_RealVM_*` hung at VM entry as of the 2026-07-29 Linux investigation (ADR-010); `mgit-guest` itself has since been fixed to boot under libkrun (MS_MOVE ordering, guest-base validation — see CHANGELOG), narrowing but not yet closing this gap. Not a Linux release gate; firecracker remains the default |
| Container (reduced-isolation fallback): SEC-03 hostile-guest | gated | `internal/sandboxd/backend/container/hostile_guest_test.go` (env-gated on rootless podman + `MGIT_E2E_CONTAINER_IMAGE`) |
| Sandbox lifecycle verbs: `sandbox launch` (only exercised indirectly via `mgit work --sandbox`), `sandbox land` | gated + unit | sandbox_posture.sh (land only); sandbox_test.go for flag wiring |
| `sandbox published` (SEC-09 one-way port publishing) | e2e | `cmd/mgit/sandbox_published_realvm_linux_test.go`, as of 2026-07-30 — boots a real firecracker microVM on KVM with a published port and drives the actual cobra command against it (human + `--json`). **Writing this test found a real production defect**, not a test gap: `microvm.Manager.newSandboxInfo` never copied the launch options' `PublishPorts` into the stored `SandboxInfo`, so `sandbox published` (and `status --json`'s `publish_ports` field) reported empty for every real sandbox on every backend regardless of what was actually published — fixed alongside this test |
| `sandbox exec`, `shell`, `grants`/`grant`, `list`, `status`, `remove`, `image install` | unit only | cmd-level unit tests exist for each (sandbox_test.go, sandbox_grant_test.go, sandbox_shell_test.go, sandbox_image_test.go); **none of these six subcommands is invoked in any e2e script**, and the previous matrix's generic "lifecycle verbs (exec, shell, grants, image)" row didn't even name `list`/`status`/`remove`/`image install` individually. As of 2026-07-29 the unit coverage for `list`/`status`/`remove` is more complete than an earlier pass of this file recorded: `list`'s populated human-readable output, `status`'s human-readable (non-JSON) output, and `remove`'s success path (with and without `--force`) were all previously untested gaps within the "unit only" label — closed in `sandbox_test.go` (only the error-surfacing and JSON-output cases existed before). Still unit-only, not e2e — that gap stands for these six |
| `mgit sandbox claude-hook` (hidden; the PreToolUse hook a real Claude Code harness drives, MGIT-11.11.1) | e2e + unit | claude_hook_test.go (7 in-process cases with an injected fake connector: healthy/down/no-sandbox/deny-rule/non-bash/malformed-stdin/empty-cwd) plus, as of 2026-07-30, `cmd/mgit/claude_hook_realprocess_test.go` — spawns the compiled binary, real stdin JSON, real production sandbox-availability check (no daemon running, so the real "ask" fallback fires) and the non-Bash passthrough case |

## CLI surface not covered above

Of 30 top-level `cmd/mgit` commands, **20 have no dedicated cmd-level test
file at all** (`add`, `audit`, `checkout`, `cherry_pick`, `commit`, `config`,
`diff`, `docs`, `export`, `gc`, `import_cmd`, `init_cmd`, `log`, `merge`,
`restore`, `rollback`, `show`, `squash`, `status`, `worktree`) — flag
parsing, error-message text, and output formatting for these rely entirely
on whatever an e2e script happens to exercise, which for several is nothing:

| Capability / claim | Proof | Where |
|---|---|---|
| `show`, `config` (get/set/list/delete), `export`, `diff`, `gc`, `import`, `docs generate`, `merge` (as a standalone CLI command, distinct from the library call `squash --to-main` makes) | unit | package tests only — **none of these commands is invoked in any `scripts/e2e/*` script** |
| `mgit worktree prune` | unit | service-layer unit tests exist (`service_operations_test.go`); as of 2026-07-29 a cmd-level test also exists (`cmd/mgit/worktree_prune_test.go`, covering dry-run/removal/no-stale-worktrees against a real repo). Still no e2e — not named in `scripts/e2e/core_loop.sh`'s worktree add/list/remove sequence, which remains the fourth subcommand it's missing |
| `mgit version` / `mgit --version` (MGIT-40 build-info resolution: ldflags vs. `debug.ReadBuildInfo` fallback) | unit | version_test.go; used only for diagnostic printing in `lib.sh`, never asserted on as a behavioral claim in any e2e script. Not mentioned anywhere in the previous matrix |
| `mgit branch` (bare/list form) | e2e (mislabeled) | actually exercised in course_correction.sh:100 (`assert_contains "$(mgit branch)" ...`) — the previous matrix filed `branch` under a blanket "unit only" line, which undercounts real coverage for at least the list form |
| `mgit run --check` | unit | run_test.go only; the health-query path itself is never driven end-to-end |
| `mgit serve --project` | unit | serve_project_test.go only; never exercised by rest_posture.sh or any other script |
| `work --base`, `.mgit/seed-include` | unit | package tests |
| Windows core loop (no sandbox) | — | uncovered: all e2e jobs run on ubuntu; unit suite is not run on a Windows runner |
| Homebrew install channel | — | uncovered in CI (tap lives in a separate repo, `hyper-swe/homebrew-tap`); verified manually at release. As of this revision the tap formula template itself (`brew/mgit.rb`) had never actually installed `mgit-sandboxd` and pointed at the wrong repo — fixed, but the fact that it went unnoticed underscores how uncovered this channel is |

Refs: MGIT-48, MGIT-53. Companion: [MCP-PARITY.md](MCP-PARITY.md) (surface
parity), [release/RELEASE-CHECKLIST.md](release/RELEASE-CHECKLIST.md) (live
sandbox passes).
