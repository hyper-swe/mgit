# ADR-015: Routing enforcement is a tier per agent family, not a property of the sandbox

**Status:** Accepted
**Date:** 2026-08-20
**Refs:** MGIT-149, FEAT-3.71, MGIT-50, R-H296, R-H299, FR-17

## Context

Containment has two independent axes, and mgit modelled only one.

1. **Is there a guest?** — `Containment: active | requested | none`. Modelled
   since MGIT-47, reported at provisioning.
2. **Can the agent's command avoid the guest?** — not modelled at all.

Because only the first was reported, a lane where routing was enforced and a
lane where it was merely suggested printed the same line. Measured on a live
worktree with a running guest:

```
$ env PATH="/usr/bin:/bin" sh -c 'whoami; hostname -s; touch /tmp/PROOF'
vimal
M5-Macbook            -> wrote to the HOST filesystem

$ env PATH="<wt>/.mgit/shims:/usr/bin:/bin" sh -c 'whoami'
root                  <- inside the guest
```

The shims work. Nothing made an agent use them. R-H296 had meanwhile made
sandbox execution mandatory for all agent lanes, and a mandate a whole family
can silently not comply with is not a mandate.

The failure was silent in the worst direction: the agent believed it was
contained, the operator believed it was contained, and untrusted code ran on
the host. MGIT-50 had made that belief *more* credible by giving AGENTS.md a
detailed, accurate description of a guest those commands never reached.

## What is actually possible

Enumerated before designing, and verified rather than assumed. A PATH shim
cannot be enforcement: any process can reset PATH or invoke an absolute path.
What can enforce routing for a process mgit does not control:

| Mechanism | Available? | Verdict |
|---|---|---|
| Harness command hook | Claude Code, Codex, Cursor — each has one | **Adopted.** The harness invokes mgit for every shell call; the agent is not asked to cooperate. |
| PATH shim | everywhere | Advice. Retained as a fallback, never as a guarantee. |
| direnv `.envrc` | only if direnv is installed *and* the dir is `direnv allow`ed | Inert on a host without direnv. Cannot be relied on. |
| Running the agent process inside the guest | not built | **The real fix.** Filed as MGIT-153; needs egress for the model API, so it interacts with the network policy. |
| Refusing to provision without enforcement | always | Available, but wrong as a default — see below. |
| Interposing on exec (`LD_PRELOAD`/`DYLD_INSERT_LIBRARIES`, namespaces, `sandbox-exec`) | Linux only, or SIP-stripped on macOS | Rejected for v1: platform-divergent, fragile, and it confines only a process tree mgit launches — which is the MGIT-153 case anyway. |

Hook capabilities differ, and the differences are load-bearing. Verified
against vendor documentation on 2026-08-20:

| Family | Config (repo-scoped) | Can rewrite? | Can deny? | Honors `ask`? |
|---|---|---|---|---|
| Claude Code | `.claude/settings.json` | yes (`updatedInput`) | yes | **yes** |
| Codex | `.codex/hooks.json` | yes (`updatedInput`) | yes | **no — parsed, not honored** |
| Cursor | `.cursor/hooks.json` | **no** | yes (`permission`) | yes |
| Other/unknown | `.envrc` | no | no | n/a |

Two consequences that a copy-paste of the Claude adapter would have got wrong:

- **Codex's fail-closed value is `deny`, not `ask`.** Claude's unroutable
  fallback is `ask`, which is safe there because a human is prompted. Codex
  parses `ask` and ignores it, so the same code would **fail open** and run the
  command on the host — this ticket's own defect, reintroduced inside its fix.
- **Cursor's reply is snake_case** (`user_message`, `agent_message`), unlike the
  camelCase the other two use. A camelCase reply marshals fine, satisfies Go,
  and is ignored by Cursor — failing open again. Pinned by a serialization test.

## Decision

Model **Routing** as a tier per family, independent of Containment, and report
it at provisioning by name.

- **`routed`** — the harness hook rewrites every shell call to run in the guest.
  Claude Code, Codex.
- **`blocked`** — the harness hook can refuse but not rewrite, so an uncontained
  command is refused with the routed form spelled out. Cursor. Weaker in
  ergonomics, identical in the guarantee that matters: nothing reaches the host
  unannounced.
- **`advisory`** — nothing intercepts. Shims and prose only. Unknown harnesses.

`mgit work --sandbox` prints the full matrix plus a machine-parseable
`Routing: <id>=<tier>` line per family, and `--agent <family>` adds a single
verdict for the family that will actually be used.

### Does `--sandbox` refuse when routing is only advisory? No, by default.

The advisory family **is** the unknown-harness family. Refusing by default
would block every harness mgit has not written an adapter for, and would push
operators to drop `--sandbox` altogether — removing the guest as well as the
advice. A refusal that blocks a working lane is its own defect.

So the default is: provision, and state the tier loudly enough to act on.
`--require-routing` turns that verdict into a refusal for an operator who needs
a guarantee. It requires `--agent`, because without a declared family there is
no single tier to test — the worktree is wired for all of them at once.

### The claim ships with the check that falsifies it

mgit installs a hook file; it cannot make a harness load one. Codex gained
PreToolUse hooks only in v0.114, and an older build ignores `.codex/hooks.json`
silently. An agent told "you are routed" would then be uncontained while
believing the opposite — the same defect in a new place. So every enforced
tier's instructions carry `hostname; whoami`, what a failure looks like, and an
instruction to stop and report rather than continue.

## Consequences

- Two families move from advice to enforcement; one more is refused rather than
  silent. Only the unknown-harness lane remains advisory, and it says so.
- mgit now depends on three third-party hook contracts it cannot exercise in CI
  (Codex and Cursor are not installable there). Mitigation: mgit's half of each
  contract is driven as a real process in tests, never a decision value the
  vendor documents as unsupported is used, and the agent-facing text carries the
  runtime check above. A vendor changing its schema will not be caught by our
  tests — it will be caught by that check, by the agent, at the first command.
- The land gate (`require_sandbox`) is unchanged and still the host-anchored
  backstop. It protects the artifact; routing protects the host. Both are needed.
- The Claude Code path is untouched. It was enforced before this ADR and is
  enforced after it.
