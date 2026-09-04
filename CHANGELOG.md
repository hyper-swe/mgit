# Changelog

All notable changes to mgit are documented in this file.

The format is based on [Keep a Changelog](https://keepachangelog.com/en/1.1.0/),
and this project adheres to [Semantic Versioning](https://semver.org/spec/v2.0.0.html).

## [Unreleased]

### Changed

- **`grants`, `grant`, `export` and `land` refused with a hex opcode when this
  daemon could not serve them (MGIT-171).** MGIT-104's fix reached the policy
  verbs only; the other four still answered `controlproto kind 0x44 not served
  by this daemon` — which tells an operator neither that this build cannot
  serve the verb (stop trying) nor that this call failed (retry). Each now
  names the verb in the operator's words and the backend, states the fact that
  makes it unservable (no land path, no grant coordinator, no exporter — and on
  firecracker, that the worktree was delivered as a launch-time image so there
  is no host directory to export from), says what to use instead, and says
  plainly that nothing was changed.

### Fixed

- **`mgit sandbox sync --force` died on a host-deleted path the guest had
  modified (MGIT-167).** The plan promoted every conflict to an update, so a
  path the host no longer had was "delivered" from a candidate tree that did
  not contain it, and the whole all-or-nothing sync failed with a raw `stat`
  on an internal path. A conflict over a path the host no longer has is now a
  forced delete: the path is removed from the guest, reported under `deleted`,
  and still audited under `overridden`, since un-landed guest work was
  destroyed even though it was asked for.

- **`mgit sandbox sync` reported a delivery the guest could not yet read
  (MGIT-192).** The host-side read-back added in 0.6.3 (MGIT-164) hashes the
  host's copy of the guest tree, where the bytes are complete; the guest's
  kernel keeps its own view of a file for a window after its last access, so
  a host rewrite inside that window stayed invisible to it — measured on
  macOS/libkrun at 0.1 s to over 1.2 s after `sync` had returned, which is how
  a `go vet` launched right after a sync read a half-updated file. A sync now
  invalidates the guest's cached view from inside the guest and reports a
  path delivered only after the guest itself reads the staged digest, within a
  bound; a guest that never agrees is a refusal naming the paths, and a guest
  that cannot be asked (no exec channel, no `sha256sum`) is reported as
  "delivered on the host, but not verified from inside the guest" rather than
  as a success. The pre-exec sync that `mgit run` performs takes the same door.
- **A sync report bounded twice under-reported its totals (MGIT-173).** The
  path-count totals added in 0.6.4 (MGIT-160) were recomputed from the lists
  on every `Bound`, so a report that had already been capped reported the cap
  as its total — 500 where 40,000 paths had diverged — and invented a cap for
  lists that were never shortened. Latent (the daemon bounds once), but the
  model's own comment invites more callers. A recorded total is now
  authoritative: it is never below the list that carries it, and a larger
  recorded total survives a later pass, so bounding a bounded report changes
  nothing.

## [0.6.5] - 2026-09-03

**The symlink cluster, and a daemon that stops stalling.** Four defects found
by the test campaign, each shipped with a test that failed before the fix and
fails again with the fix removed. Two of them turned up a second defect in the
same seam while being fixed.

### Deleting a symlink no longer empties the file it points at (MGIT-168, #83)

A sync that removed only a link truncated the link's *target* — a path the
plan never named — and reported success with a clean `Deleted: [link.txt]`.
`removeForGuest` emptied files before unlinking them (the MGIT-90 dentry
argument), and `os.Truncate` follows links. Only a regular file is emptied
now: the kind is read with `Lstat` and made binding at the open with
`O_NOFOLLOW`, so a guest swapping the entry for a link between the two calls
gets `ELOOP` rather than a write through it.

### A worktree with an internal symlink can be synced again (MGIT-165, #84)

Sync flattened a link into a copy of its target, and the delivery read-back
(0.6.4) then refused the whole sync as stale content — so any repository with
a monorepo package link or `docs/x.md -> ../x.md` could be launched but never
synced. A link is now delivered as a link, with the same target text the
manifest hashes. **Second defect, same seam:** the destination was opened
with `O_TRUNC`, which follows links too, so a forced sync onto a path where
the guest had planted a link wrote *through* it — resolved on the host, by
the daemon, wherever the guest chose to point it. Nothing is written through
a link now; the destination is cleared unless it is a regular file and the
open carries `O_NOFOLLOW`.

### A dangling in-tree link is no longer refused as an escape under `/tmp` (MGIT-166, #85)

On macOS `/tmp` and `/var/folders` sit behind symlinks, so every
`mgit work /tmp/...` worktree does. The SEC-03 guard canonicalised the root
but only an *existing* target, so a link to a not-yet-generated in-tree path
(`dist -> build/out`, a `.bin` shim) compared two namespaces and was refused
with a containment message about a breach that did not exist. The target's
longest existing ancestor is now resolved and the missing tail re-appended.
**Second defect, same change:** under a direct root the per-link check had
accepted a dangling leaf beneath an in-tree directory link that points
outside; it is refused now.

### The snapshot pass runs off the daemon's request loop (MGIT-170, #87)

The passive snapshot pass ran inline in the daemon's select loop, so every
connection accepted during one waited for the whole remaining pass, and a
shutdown waited for a pass — or several, once a pass outlasted the tick.
Measured with an uninterruptible watcher: first byte of the greeting 1.50s
behind a 1.5s pass, shutdown 2.00s (4.00s in a second run) behind a 2s pass.
The pass now runs on its own goroutine under a single-flight guard; after:
397µs and 236µs. Shutdown does not wait for an in-flight pass, by decision
(a snapshot is housekeeping and the drain is not), and says so in the log.

### `mgit doctor`

- **`base/currency`** (MGIT-174, #79 — merged after 0.6.4 was tagged, so this
  is its first release): a guest base now records which mgit composed it, as
  a deterministic marker inside the digested tree, and doctor compares it
  with the substrate running it. An unrecorded base is reported as a
  **failure**, not a pass: for two releases the absence of a warning had been
  read as an assurance while a stale base silently ran older guest binaries.
  **Upgrade note:** every base composed by a release before this one carries
  no marker, so `mgit doctor` will fail `base/currency` on it until you run
  `mgit sandbox base from <image>` again. That is the check working, not a
  regression.
- **`daemon/response-cap`** (MGIT-175, #88): asks the running daemon for a
  full 1 MiB control response and verifies it arrived intact, then asks for
  one byte more and verifies the refusal is legible — the MGIT-160 incident,
  checked on the real channel. The control-plane protocol version is now 3; a
  daemon left running from an older build is refused at the handshake with
  the restart named, and doctor reports `not-checked` with that reason.
- **`guest/localhost`** reads the guest's name table (`/etc/hosts`) instead of
  running a resolver, so it can no longer pass via DNS; a base without the
  probe command reports `not-checked`, never `failed` (MGIT-169, #89).

### `mgit log`

- `--since` and `--until` refuse a value they cannot parse, naming the flag,
  the value and an acceptable form (MGIT-172, #90). A plain date or a
  relative phrase used to be silently discarded, and the reviewer was handed
  the whole trail with nothing to say the narrowing had not happened.

### Build hygiene

- goreleaser is pinned (v2.17.1) on the release path (MGIT-179, #81): `@latest`
  resolved to a release whose Go floor the runner does not meet, broke a
  documentation-only PR, and on the release path would have burned a tag
  number, since a tag never points twice.
- CI refuses a build tool or action resolved at a floating ref unless the
  float is declared at the site with its reason; the tree passes with the two
  `govulncheck` floats declared (MGIT-180, #86).
- The branch-scope guard reads linked worktrees, and the pre-push hook no
  longer treats "could not run" as a refusal (MGIT-182, #92).

### Known issues carried

- MGIT-164: a sync that reported success while the guest lacked files was
  never reproduced; the class is refused by the read-back since 0.6.4, and
  none of the four fixes above produces that shape.
- MGIT-158: there is still no daemon↔guest version exchange.
- MGIT-181: whether a sync should police paths outside its plan is an open
  design question.

## [0.6.4] - 2026-08-23

**A sync now verifies its own delivery.** The release theme is the same as
0.6.3's, one layer deeper: not just failing legibly, but refusing to claim work
that did not happen.

### Sync reads the delivery back before reporting it

A worktree sync reported success while the guest's tree lacked the files — a
`git apply` created a file and modified its sibling, and the guest could read
neither. mgit had no idea. It was caught by a consumer's stale-copy check one
layer up.

That is the silent-destruction class, and it is the worst kind: a sync that
fails loudly costs a retry, while a sync that reports success sends an agent on
to build against a tree missing its own changes, and everything derived from it
inherits the error without a symptom.

**The root cause of that instance is still unreproduced**, and this release
does not claim to fix it. Four shapes were tested live and all behaved
correctly; both code suspects were cleared by reading. What ships instead makes
the whole CLASS loud regardless of which mechanism drops an operation: `Apply`
returning without error says the writes were ATTEMPTED, not that the guest can
read them. The guest tree is now read back and compared against what was
staged, before anything is reported — and before the delivery baseline moves,
so an undelivered sync re-derives the same work next time rather than recording
a delivery that never happened.

Only planned paths are checked. A guest is entitled to its own files, and
policing paths outside the plan would refuse syncs over work the guest
legitimately created.

### `mgit doctor`

Standing checks for conditions that have already cost someone a diagnosis. Its
one load-bearing decision: **not-checked is a first-class verdict**, distinct
from both ok and failed. A diagnostic that silently skips is worse than no
diagnostic — silence reads as a pass, and the reader has been misled by the
instrument they consulted to avoid being misled. A check that cannot run says
so, says why, and does not fail the exit code.

Every failure names a remedy. Every check names the incident it converts, so
the reason it exists outlives the people who remember it. The first two:
whether this repository's recorded tree contains a nested `.git` (answerable at
rest, where it previously required a failure), and whether the guest resolves
localhost without a DNS query.

### The daemon asked the wrong layer whether anything was registered

The idle check consulted the BACKEND, which knows only booted VMs — while
registration is a service-layer fact. So a task registered but not yet used had
no VM at all, and the daemon exited while a live task was bound to it, stopping
everything it does continuously. Its own doc comment already said "registered"
while the code asked the backend; the two had drifted with nothing to make them
disagree out loud.

Frugality is preserved and was verified as carefully as the fix: a daemon with
nothing registered still exits on its idle grace.

### A task's trail says what each commit was

`mgit log --task-id` — the reviewer's view of what an agent did — printed the
index position (`pos=0`, `pos=1`) rather than the commit subject. The messages
were correct and complete; the task-scoped log simply read an index that does
not store them. The subject now comes from the commit object, the authoritative
copy, so there is no second copy to drift. A commit whose object cannot be read
is reported as unavailable rather than blank, because a blank reads as an empty
commit message, which is a different fact.

### CI no longer refetches a 145 MB kernel on every run

A pinned kernel tarball was downloaded from a third party before every libkrun
build, putting a transfer to a rented runner on the critical path of the whole
repository — it blocked an unrelated change three times in a row. The tarball is
pinned, so its identity is its content, so it is cacheable by construction. It
is verified on the way into the cache and on the way out, because caching must
not weaken the thing it speeds up.

## [0.6.3] - 2026-08-22

**The release of things that failed silently.** Every headline entry here is a
failure that gave its user the wrong signpost, or none at all.

### A crash became a refusal

`mgit sandbox sync` died with `sync classification failed: read response: EOF`
— four times across three work items on a walk. The cause, three hops:
a control response over 1 MiB was **refused and never written**, the daemon
logged that to itself, and the deferred close handed the caller a bare EOF. So
the only record of what happened lived inside the daemon, and the client learned
neither the cause nor a next step.

That is now a legible refusal naming the size, the cap and what to narrow. The
over-size case is a sentinel rather than a message, because a daemon has to tell
it from a dead connection — one can still be answered, the other cannot — and
matching on prose breaks the moment the prose improves.

The classification itself is **bounded by construction**: it carries full counts
always, and marks itself truncated when the path lists were shortened. So an
unmarked report means exactly one thing — nothing was dropped. The asymmetry is
deliberate. A silently shortened list of diverged paths would be *believed*,
which is worse than the crash it replaces, because a crash is not believed.

A worktree carrying a host-side `node_modules` is the ordinary case that broke
it: 20,000 diverged paths is far more than 1 MiB of JSON holds.

### The guest could not resolve localhost

The guest shipped **no `/etc/hosts` at all** — absent, not incomplete — so every
`localhost` lookup fell through to DNS, and under the default deny-all egress
that lookup cannot succeed. vitest, vite, jest, dev servers, anything that binds
or dials a local port: in practice every JS project, failing inside a sandbox
that was working exactly as designed.

mgit composes its guest userspace from an OCI image, and images ship no hosts
file because the *runtime* writes one. Docker does. Podman does. mgit is the
runtime here and did not. Nothing was deleted — the file was never created.

The error made it worse than it had to be: `EAI_AGAIN` says *DNS*, so it points
at the egress policy and sends the reader somewhere else entirely.

Now written at guest init, before the NIC and regardless of network mode —
loopback resolution is not a network feature, and deny-all is precisely the mode
that needs it. An image with its own table keeps it untouched.

### Found by walking the product

- **A nested `.git` bricked a repository.** The working-tree walk excluded `.git`
  by its top-level component only, so a vendored checkout or a git fixture under
  test data — `testdata/inner/.git/HEAD` — was absorbed into the base. Every
  later `mgit work` then died inside go-git's tree walker, which refuses `.git`
  as a path component: a correct guard firing on content that should never have
  reached it. Reported as a `/tmp`-versus-home failure; it is neither, and the
  same repo fails identically under `$HOME` with no symlink in the path.
  A repository already in that state now gets told what happened and how to
  recover, instead of a chain of internal verb names.

- **The toolchain was invisible.** A base that installs its toolchain outside the
  distro's default directories had it unreachable — `go: command not found`
  inside a sandbox that contained Go. An OCI image already declares its own PATH;
  mgit now believes the image rather than carrying opinions about toolchains.
  Only PATH is taken, whole or not at all.

- **Two verbs disagreed about where they were.** `mgit run` started in the
  worktree, `mgit sandbox exec` in `/`. Both defensible alone, neither decided.
  The sandbox is bound to a worktree; that is the answer, and it is now given by
  the service so every client gets the same one.

- **A graceful shutdown wrote the audit trail of a crash.** The drain reached the
  backend directly, while the terminal `destroyed` event is written a layer
  above, so an orderly stop left no terminal event and the next daemon stamped it
  `killed / unsupervised`. Since the daemon's own idle exit takes that path, the
  common case was manufacturing crash records.

- **Containment was enforced for one agent family and advised for the rest.**
  Codex and Cursor have gained shell hooks; both are now enforced. Only an
  unknown harness remains advisory, and it says so.

- **Recovery stopped depending on the agent's diligence.** Passive worktree
  snapshots record a settled worktree with no agent discipline at all, under
  their own ref namespace as orphan commits — unreachable from any task branch,
  so squash and land exclude them by construction rather than by rule.

- **An unenforceable network mode is refused when you configure it**, not
  minutes later at first use, and the egress-policy verbs now name the
  containment fact rather than a wire frame tag.

## [0.6.2] - 2026-08-20

**The in-tree era ends.** A guest base no longer lives inside your repository.

Provisioning used to unpack the base into the repo's own `.mgit/` tree — 175 MB
and 5,240 files here, 906 MB in a consumer's repo — where any command that walks
the tree found it. Their `gofmt -l .` red-lit on hundreds of files that were
mgit's artifact, not theirs. A containment tool that breaks the host
repository's own test command fails at the worst possible moment: the first
thing a newly-sandboxed lane runs.

It was also **mutable**, which is the worse half and the one nobody reported:
recomposing in one worktree silently invalidated the pinned digest for every
other worktree on the machine.

Now there is one base per **digest**, machine-wide, outside every repository,
immutable by construction. A repo stores a reference and no bytes. Drift is
impossible rather than detected. Tags are recorded as provenance, never as
identity — a base digest was observed changing under an unchanged tag, and a
digest is what makes that ambiguity unable to matter.

A missing cache entry **fails closed** rather than silently refetching. A
recompose is not guaranteed to reproduce the pinned digest, because the composed
tree includes the guest binaries the running mgit build injects; an automatic
refetch could only fail the same check, or tempt someone into accepting
different bytes under an old pin.

### Containment: what is enforced, and what is advised

The agent-facing instructions used to tell every agent family that its commands
run inside a hardware-isolated microVM and "they are contained". That holds for
**Claude Code**, whose every tool call is routed into the guest by a harness
hook it cannot bypass. It does **not** hold for Codex, Cursor, or the generic
adapter, whose only routing mechanism is a PATH shim — and a PATH shim is
advice. Any process can reset PATH or call an absolute path, after which the
command runs on the host, uncontained, with no warning. Measured, not inferred.

The instructions now state that split for the family reading them, and hand over
a check rather than asking for trust:

```
hostname; whoami        # in the guest: a container name, and root
                        # on the host: your machine, and your user
```

**Enforcement for those families is not in this release.** Until it lands, treat
sandbox execution as enforced for Claude Code and advisory for every other
family. The wording is corrected now rather than when the mechanism arrives,
because a containment guarantee with nothing behind it is believed by the
operator as readily as by the agent — and the more precise the prose, the more
readily. This block had recently been made *more* detailed and convincing, which
made it more wrong for most of its readers, not less.

### Also

Every third-party fetch in CI is now guarded — bounded retry, a per-step
timeout, and a precondition restore between attempts so a retry is a fresh
attempt rather than the same one repeated against a half-written artifact. The
exception list is a script that fails on an unguarded fetch, not a document that
asks to be kept current.

## [0.6.1] - 2026-08-20

> **0.6.0 was tagged and never published, and is superseded by this release.**
> Its preflight failed on a coexistence assertion that mgit had not broken —
> git's own background maintenance wrote `objects/maintenance.lock` between the
> test's before and after snapshots, and the test attributed that write to us.
> The tag was deleted rather than moved: a tag that never points twice makes
> drift impossible rather than detected, which is the same rule this release
> applies to the guest base's digest. No artifact was ever published under it.


The substrate release. Cut because sandbox execution became mandatory for every
agent lane, and v0.5.0 could not carry that: on it, **any command running longer
than 30 seconds was killed** — the daemon buffers output and relays it on
completion, so a silent test suite died at exactly 30.0s. A lane told to run its
tests in a sandbox would have concluded the substrate could not run tests.

### Upgrading — read this first

**Install from a release archive, not `go install`.** The Linux guest binaries
(`mgit`, `mgit-guest`) ship only in `libexec/guest/`, and `mgit sandbox base
from <image>` cannot compose a guest base without them.

**Remove any older `mgit-sandboxd` from your PATH.** The CLI spawns whichever
daemon it finds first, and a stale Homebrew copy can precede a fresh install.
This release refuses a mismatched pair loudly instead of misbehaving quietly
(see the handshake below), but a refusal you do not know how to clear is still
friction:

    brew uninstall mgit        # if an older tap copy is shadowing the install
    which -a mgit mgit-sandboxd    # both must resolve to the same install

**CLI and daemon must now match.** The control protocol carries a version and a
mixed pair is refused, naming both builds and the upgrade routes. This is a
deliberate break: this codec has no negotiation seam, so a version range would
be a promise it could not keep.

### Known limitation

**The guest base unpacks in-tree** at `.mgit/sandbox/base/` (~900 MB per repo).
It is gitignored, so there is no commit risk, but a test command that walks the
tree (`gofmt -l .`, linters, file counts) will see it. It is also MUTABLE:
recomposing it in one worktree invalidates the pinned digest for every other.
A content-addressed machine-wide cache is the fix and is next.

### Added

- **`mgit squash` and `mgit merge` take `-m` and `-F`, so a message with
  backticks or `$(...)` never goes through the shell (MGIT-106).** Both verbs
  had only `--message` — no shorthand, no file — so composing anything richer
  than a single clause meant `--message "$(cat msg.txt)"`, which hands the
  shell responsibility for an audit artifact whose failure modes are silent
  truncation and mangling rather than a loud refusal. That mattered more here
  than it did for `mgit commit` (MGIT-105): the squash message is the one
  message that leaves mgit's store for the user's **real** git via `--to-git`,
  so a quoting accident there escapes into a repository mgit does not own.

  Both now accept `-m/--message` inline or `--file/-F <path>`, which reads the
  message as bytes verbatim (`-` reads stdin). The two are mutually exclusive
  and passing both is refused **naming both flags** — silently preferring one
  would leave the caller believing it recorded something the record does not
  say. An unreadable or empty file fails before the repository is opened, so a
  failed `-F` squashes nothing, merges nothing and leaves nothing staged.

  Verified end to end rather than by unit test alone: a message carrying
  backticks, both quote kinds, `$(...)`, a tab, consecutive blank lines and a
  trailing blank line comes back out of `squash -F … --to-git` with the same
  SHA-256 it went in with, and `git am` on that patch records the caller's
  subject line exactly — git strips the `[PATCH]`/`[squashed]` envelope, so no
  mgit marker reaches the reviewer's history.

  `mgit rollback --reason` deliberately did **not** get the same treatment: a
  reason is interpolated into a generated one-line summary, not recorded as a
  message, so a byte-identity promise there would be a lie. The rationale is
  recorded at the flag.

### Changed

- **A squash message you supply is now recorded verbatim (MGIT-106).**
  `mgit squash --message`/`-F` previously had mgit's own micro-commit summary
  appended to it, so the recorded message said more than the caller wrote —
  the same defect class the message-file flags exist to close, and what stood
  between `squash -F` and byte identity. The summary still trails the message
  mgit generates for itself when you supply none; `mgit log --task-id <ID>`
  remains the full provenance either way.

- **`mgit commit` no longer accepts `--m` as a long flag (MGIT-106).** `-m` was
  registered as a second flag named `m` rather than as `--message`'s shorthand,
  which incidentally made `--m MSG` work. All three verbs now share one binder,
  so `-m` and `--message` are one flag. `-m` and `--message` are unaffected;
  only the undocumented `--m` spelling is gone.

- **A branch that carries another branch's commits is refused before its pull
  request exists (MGIT-142).** `git checkout -b` silently inherits whatever
  branch you are standing on, and on 2026-08-19 that put `9abf4ce` on main
  describing a 24-line change to one shell script while actually carrying 531
  lines across six files from an unmerged task branch. The diff showed all six
  files and the PR was merged anyway, so the fix is mechanical rather than
  attentional: `scripts/hooks/pre-push` (installed by `make install-hooks`)
  refuses the push, naming the parent branch, the commits, and the files they
  bring into the diff; CI repeats the check on every pull request as a backstop.

  The rule is deliberately the cheap one — *a branch may not carry commits that
  belong to another unmerged branch* — and needs no ticket metadata, no declared
  file list and no per-task plumbing. It was measured before it was wired to
  anything: over all 54 branches in this repository (41 of them merged pull
  request heads) it refuses exactly one, the incident, with zero false
  positives. A deliberate stack declares its parent for one run
  (`make branch-check BASE=…`), or records why permanently with a
  `Branch-Scope-Override:` commit trailer that travels in the history the
  reviewer reads. Developer tooling only: no shipped binary changes.

- **`mgit log --task-id` labels a task's commit trail as process history or
  post-hoc packaging (MGIT-110).** Measured across five real sub-agent runs on
  this repo, agents treat `mgit commit` as a final packaging step: MGIT-102's
  seven index rows look like a healthy micro-commit trail until you notice the
  six authored ones were all written in the last five minutes of a forty-minute
  run, thirteen seconds apart. A reviewer reading six commits believes they are
  seeing six coherent steps; they are seeing one step split six ways after the
  fact. That is a provenance claim the record does not support, and the label
  converts it from an unfixed problem into a disclosed one.

  The verdict rests on a **denominator**, because a commit window on its own
  says nothing — six minutes of commits is a burst across a forty-minute run
  and is *complete coverage* of a six-minute one. The denominator is the
  worktree's `created_at` in the worktrees registry (written by `mgit work`)
  through to the last authored commit, published with every verdict as
  `WORKTREE_CREATED_TO_LAST_COMMIT`. Squash and merge commits are excluded from
  the trail: they restate existing work at hand-off, so counting them both
  inflates the count and drags the window to the end of the run.

  A manufactured trail turns out to be the *rarer* problem. Across the same six
  runs, one was packaged post-hoc, two were genuinely spread, two recorded a
  **single commit**, and one recorded **nothing at all** — half the runs left no
  process history whatever. So one commit closing a 33-minute run gets its own
  verdict, `SINGLE_CHECKPOINT`, rather than being pooled into "cannot tell":
  that is not a measurement that failed, it is the complete observation that
  there is no earlier point in the run to return to. It is gated on the same
  denominator trust as every other verdict, so one commit ending a three-minute
  run stays unremarkable and unlabeled.

  Every way that denominator stops measuring one agent run is a **refusal**,
  never a verdict — no worktree registered, a trail spanning more than one
  session, commits older than the worktree holding them, or a run too short to
  read anything into its shape. Every refusal is a failure of the *denominator*;
  none is about the commit count. `INSUFFICIENT_EVIDENCE` always carries a
  reason and never means "fine".

  The tokens (`PACKAGED_POST_HOC`, `SPREAD_ACROSS_RUN`, `SINGLE_CHECKPOINT`,
  `NO_COMMITS`, `INSUFFICIENT_EVIDENCE`) are a closed, golden-tested contract,
  readable from `cadence.verdict` under `--json` and from the head of the human
  footer; the summary prose is explicitly not contract. The closed-set test
  holds the set closed from both sides — nothing may invent a token, and no
  published token may be unreachable. It is **evidence, not a score**: there is
  no percentage, no ranking, and nothing in mgit gates on it, because an agent
  committing to satisfy a checker manufactures exactly the trail the label
  exists to expose.

### Changed

- **BREAKING: `mgit` and `mgit-sandboxd` must now be upgraded TOGETHER
  (MGIT-136).** The CLI↔daemon control plane has gained a version handshake,
  and a mixed pair is refused with a message naming both builds and the
  command to fix it, instead of failing later as something it is not.

  Upgrading only one side — most easily, upgrading the binaries while the
  previous release's daemon is still running — now produces:

  ```
  mgit CLI and daemon differ — upgrade both.
    mgit CLI:      control protocol 2, 0.6.0 (commit: …)
    mgit-sandboxd: control protocol 1, build not reported (too old to say)
  ```

  followed by one upgrade command per install route and `pkill -f
  mgit-sandboxd` to release the stale daemon. **After upgrading, stop any
  running `mgit-sandboxd`**; the next command starts the new one.

  This is a deliberate trade. `internal/controlproto` decodes requests *and*
  responses with `DisallowUnknownFields` and the exec frame tags are a closed
  set, so the wire has no forward compatibility and every addition to it has
  silently broken one version direction — `PolicyResult.Pending` and
  `Response.ErrorCode` (MGIT-109), `Response.Synced` (MGIT-76), and the
  MGIT-133 liveness beat. The last was measured reporting a pure wire mismatch
  to the operator as **in-guest memory exhaustion**, which is the misdiagnosis
  class this project has now fixed four times (MGIT-104, MGIT-108, MGIT-118,
  MGIT-136). A refusal that says what it is beats a pair that works until it
  silently does not. The rule is normative in
  `IDD-FR17-SANDBOX-PROTOCOL.md` §8: `controlproto.ProtocolVersion` is bumped
  in the same commit as any wire change, and compatibility is exact equality.

  Compatibility of the FILE formats, the store, and every non-sandbox verb is
  unaffected — this is the CLI↔daemon socket only.

  Two follow-ups reconcile that handshake with what it made unreachable
  (MGIT-138):

  - **A silent exec stream is now always a daemon stall.** MGIT-133 read a
    stream carrying no liveness beat as "an `mgit-sandboxd` older than
    MGIT-133": the stall deadline was dropped, the unbounded wait restored,
    and a notice printed. A daemon that old is now refused at the handshake
    and never reaches the exec stream, and the one case that still could —
    a current daemon wedged before its first beat — was answered wrongly by
    that fallback twice over: it named the daemon *old* when it was *hung*,
    and it reinstated the unbounded wait MGIT-122 and MGIT-133 exist to end.
    A peer that reaches the stream has stated it speaks the beat, and the
    daemon emits its first one before any guest work begins, so silence is
    now reported as `ErrSandboxDaemonUnresponsive` whether or not a beat
    came first.
  - **The mismatch message aims its closing action at the stale side.**
    Both peers exchange both versions, but both used to end with `pkill -f
    mgit-sandboxd` regardless. That is exact when the daemon is the old
    half; when the CLI is the old half it sent the reader after a process
    that was already current. That line is now rendered only when the
    daemon is behind; when the CLI is behind the message says so and names
    upgrading the CLI as the whole fix. Both builds, every install route,
    and the closing `mgit --version; mgit-sandboxd --version` are unchanged
    in both directions.

- **BREAKING: `mgit log --task-id <ID> --json` emits an object**
  `{"task_id", "commits", "cadence"}` instead of a bare array of commit
  records. Callers reading the array should read `.commits`. Plain
  `mgit log --json` is unchanged.

### Fixed

- **A launch refused for want of HOST capacity is no longer reported as the
  GUEST running out of memory (MGIT-118).** A sandbox refused by the aggregate
  fleet ceiling printed the correct refusal — "this launch is not too big; free
  capacity" — and then, two lines below it, a second paragraph claiming "the
  guest stopped answering mid-command", naming this sandbox's cap, and offering
  a remedy that **doubled** its `--memory-mb`. No VM had been started at all,
  and raising the sandbox's memory makes a host-wide refusal *more* likely, not
  less: an agent following the last, most actionable-looking paragraph frees one
  sandbox's worth of capacity and then asks for twice as much, so the loop
  oscillates instead of converging. Reproduced live on macOS/libkrun with an
  819 MB ceiling and 512 MB sandboxes; the refusal now reads

  ```
  mgit: the host refused to admit this sandbox (task C-2), so no VM was started
  and no command ran — this is the HOST's capacity, not a fault of your workload
  or of a guest.
  ```

  and the freeing-then-retry it advises was measured converging on the first
  attempt.

  The larger half of the fix is the **default**. `phaseLostServing` — the one
  phase that carries the MGIT-95 memory-cap advisory — was what the classifier
  said when it did not recognize a failure, and four separate causes had to be
  carved out of it by name after each was reported to a user as in-guest memory
  exhaustion: a VM that never booted (MGIT-104), this ceiling refusal, a stalled
  daemon (MGIT-133) and a wire-version mismatch (MGIT-136). A default that
  asserts a specific diagnosis is wrong every time reality adds a case, so every
  phase now requires positive evidence of its own — `phaseLostServing` included,
  via the transport markers only a guest that *had* answered can produce — and
  anything else is reported as unidentified: the evidence, where to look next,
  and no cause. That branch is the zero value, so a phase nobody assigned cannot
  diagnose anything either. A client-side socket timeout on a healthy sandbox
  (MGIT-122) lands there now instead of in the memory advisory.

  The MGIT-95 advisory is unchanged where it is right: a signal exit (137/134)
  and a guest genuinely lost mid-command still name the cap in force and still
  carry "do not reshape the build to fit the sandbox".
  `scripts/e2e/sandbox_fleet_soak.sh` phase 3 asserts this as a hard invariant —
  it was the last `known_defect` in that gate, which now reports a clean PASS.

- **Concurrent `mgit work` no longer times out on the repo lock (MGIT-120).**
  `mgit work` held the repo-wide exclusive lock for its whole lifetime — across
  a full working-tree fingerprint, the worktree materialization *and* the
  `mgit-sandboxd` round-trip — so the worker-pool shape every fleet starts with
  serialized and then failed: the fleet soak measured concurrent provisions
  dying with `another mgit process is running: held by PID ... after 30s`. The
  tell was that `mgit sandbox launch` performs the same daemon registration
  while taking no repo lock at all.

  Provisioning now runs in two phases: a **locked claim** (base resync, task
  branch, registry insert) and an **unlocked materialize** (marker + branch tree
  into the worktree's own path, then the sandbox launch). The narrowing is safe
  because the lock was never what enforced FR-16's exclusivity rules — the
  registry's `UNIQUE(path)`/`UNIQUE(task_id)`/`UNIQUE(branch_name)` constraints
  are, and the insert precedes any disk write, so exactly one racer wins and
  owns its path/task/branch for the whole unlocked phase. Race losers are now
  refused *by name* (`task already bound to a worktree: T-1 (held by worktree
  wt-a)`) instead of by a raw SQLite constraint string.
  [ADR-009](docs/adr/009-per-operation-locking.md) is amended accordingly: the
  lock is scoped to a shared-store mutation, never to a process — server or CLI.

  Two defects found alongside it are fixed too. A worktree materialized
  **inside** the repo root was walked as project content by every later
  fingerprint (quadratic as a pool warms up) and absorbed into the shared base,
  so each new worktree carried a copy of every earlier one; the walk now skips
  any nested mgit root wherever it sits. And `locks.timeout_seconds` — promised
  by REQUIREMENTS.md while the code carried a compile-time constant — is now
  real (default 30s unchanged, capped at 3600s), which also required
  `mgit config set` to stop rejecting numeric and boolean values.

  Measured on a loaded scratch repo: before, 2 of 4 concurrent provisions failed
  and the winner took 65s; after, 6 of 6 succeed in 1–2s each.

- **`mgit export --format git` now emits a real patch (MGIT-112).** It rendered
  a syntactically valid mbox with ZERO hunks: the export correctly squashed in
  dry-run so it would not mutate state, then rendered from `FileDiffs` that
  only the non-dry-run path ever populated. `git apply --allow-empty` and
  `git am --allow-empty` accept that patch, exit 0 and change nothing — silent
  loss on the verb whose only job is getting work out of mgit, the MGIT-77
  failure shape one layer down. It was found by a consumer *during a work-item
  recovery*, which is the worst possible moment to reach for an export.

  The export now computes the task's net result **tree** over its base and
  diffs it through the same go-git encoder `mgit squash --to-git` uses, so the
  two verbs agree on hunks by construction rather than by coincidence. It still
  creates no squash commit, indexes nothing, audits nothing and moves no ref —
  its read semantics were right all along; only the rendering was wrong.

  Three outcomes are now distinguishable, which matters most mid-recovery:
  a real change prints the patch and exits 0; a genuinely empty net change
  (a change and its revert) prints **no patch** and an explicit note on stderr
  and exits 0; anything uncomputable **exits non-zero**. `--output` writes no
  file for an empty net change rather than a misleading empty one, and a
  non-empty task that somehow renders no hunks is refused outright.

- **`mgit squash --to-git --dry-run` is a real preview.** It previously failed
  with `commit not found: to commit is empty`, because there was no squash
  commit to diff. It now renders through the same read-only path as the export.

- **The test that missed MGIT-112 was unmasked.** `TestExport_Git` asserted only
  on mbox header lines, every one of which a header-only patch satisfies. It now
  asserts on hunks and applied file content, and the new CLI tests check the
  round trip by reading files back after `git apply` — never by trusting its
  exit code, which succeeds against the bug.

## [0.5.0] - 2026-08-15

> **Update:** the known limitation below (MGIT-112) is FIXED in 0.6.1.

The containment-integrity release. Everything below came from USING mgit — a
consumer's agent walk, an interrupted recovery, and our own dogfooding — rather
than from reading it. Two of the defects were found because a sub-agent
complained about something that looked like a stale ticket.

### Known limitation in this release

**`mgit export --format git` emits a header-only patch with no hunks
(MGIT-112).** It runs the squash in dry-run so the export does not mutate
state, but the git rendering reads file diffs that only the non-dry-run path
populates, so the patch carries an mbox header and no content at all. How that
presents depends on your git: **git 2.42+ rejects it loudly** (`No valid
patches in input`, exit 128 from both `apply` and `am`, with `am` additionally
leaving a stuck am-state), while an older git — or any `git apply
--allow-empty` — accepts it, changes nothing, and exits 0. So the loss is loud
on a current git and silent on an old one, and either way the work does not
arrive. **Use `mgit squash --task-id <ID> --to-git` instead**, which is correct
and is the path the agent documentation has always specified. This predates
v0.4.5 and is not a regression in this release. Fix is P1 and next.

### Added

- **Egress-policy failures now carry a stable, machine-readable code.** Every
  failure of `mgit sandbox policy set` / `revoke` / `show` reports one of
  `NOT_BOOTED`, `BOOTED_DIED`, `VERSION_PREDATES` or `UNKNOWN` — as `error_code`
  in `--json` output, in the MCP tool's error result, and in square brackets at
  the start of the human message. **These tokens are a stable contract; the
  prose beside them is not.** An integrator built a pre-boot retry by matching
  on the error wording, and it silently missed the failure below entirely;
  rewording it would have broken them a second time, just as silently. Match on
  the token. The set is closed, and a cause this build cannot classify gets
  `UNKNOWN` rather than the nearest of the other three — a confident wrong
  answer is worse than an admitted one. A golden test pins the exact strings.
  (MGIT-109, R-H233)

### Fixed

- **A SIGKILLed or crashed `mgit-sandboxd` no longer orphans its microVMs.**
  Ordinary daemon exits — idle timeout, SIGINT, SIGTERM — drain, stopping and
  removing every sandbox, and always did. The ungraceful ones do not: a SIGKILL,
  an OOM kill or a crash simply ends the process, and its VM children were
  reparented to init and kept running — holding their memory, their staged copy
  of the worktree and their per-VM sockets, addressable by no daemon (the
  replacement has no handle, so `stop` and `remove` could not reach them) and
  killable only by hand. Measured on macOS/libkrun before the fix: `kill -9` of
  the daemon left the VM child alive at 54 MB RSS with the worktree still
  mounted. The fix is kernel-enforced on both GA backends, because a host-side
  cleanup is exactly what a SIGKILLed process is not around to run: on
  **libkrun** (macOS and Linux) the VM child — mgit's own binary — inherits a
  *lifeline* descriptor whose other end the daemon holds, and the kernel closing
  it on the daemon's death is what ends the VM. The child proves the descriptor
  really is its daemon's before trusting it — right file type, carrying a per-VM
  nonce the daemon wrote into it — because an inherited environment variable
  naming a descriptor number is a claim rather than evidence, and acting on the
  claim alone lets a process read an unrelated descriptor and announce that its
  daemon died. On **firecracker**, whose VMM is
  a foreign binary that watches nothing, the VMM gets `PR_SET_PDEATHSIG` and is
  forked from a pinned OS thread, since Linux keys that signal on the forking
  *thread's* exit and a Go scheduler free to retire that thread would otherwise
  kill healthy VMs. Asserted with a real booted VM and a process count in
  `scripts/e2e/sandbox_registry_durability.sh`. **Not covered:** the
  `--backend container` reduced-isolation fallback, whose containers podman
  owns. This was a supervision and resource leak, never a containment breach —
  on libkrun the egress authorizer lives in the VM child, so an orphan's network
  policy went on being enforced. (MGIT-103, R-H227, FR-17.19, NFR-17.6)
- **`sandbox policy set/show/revoke` failed against a sandbox that lazy
  provisioning had deliberately not booted.** The verbs dialed the VM's control
  socket unconditionally, so on the documented setup path — `mgit work
  --sandbox --network allowlist`, then the egress-policy step — they failed with
  `vm control channel unreachable … c.sock: no such file or directory` against a
  sandbox that was correctly in state `created`. Provisioning is lazy by design
  (FR-17.9, FR-17.10): the microVM boots on first use. The policy is now
  **staged onto the pending launch** instead, durably, and the VM comes up
  already enforcing it — which is not merely a workaround but strictly safer
  than booting a guest in order to reconfigure it, since the VM never runs under
  the replaced policy even momentarily. `policy show` reports a staged policy as
  `PENDING` and never as one in force. A suspended sandbox is handled the same
  way, and a boot landing mid-stage is refused with nothing staged and re-routed
  to the live enforcer, so a policy can never be reported as enforced when it is
  only pending. Verified live on macOS/libkrun: the replaced launch-time
  destination is genuinely refused by the booted guest, and the staged one reads
  back from the running enforcer. (MGIT-109, R-H232, FR-17.9, FR-17.10)

- **The unreachable-enforcer message guessed between three different
  failures.** It said "the sandbox may not be running, or its VM predates this
  capability" — one shrug covering a sandbox that never booted, a guest that
  died (MGIT-99), and a VM launched before the control channel existed
  (MGIT-74). Each has a different remedy, and that string is why this very bug
  was reported against two unrelated tickets (MGIT-102, MGIT-103). The daemon
  holds the recorded state and the enforcer reports host-side evidence; between
  them the condition is known, so it is now named, with its own code and remedy.
  Every one of these still fails closed with the policy unchanged: an
  unreachable enforcer is an error, never an empty policy. (MGIT-109, MGIT-104)

- **The fleet-wide memory ceiling was inert in a default install; it is now
  resolved from host policy.** `mgit-sandboxd` wired the FR-17.26 aggregate
  memory ceiling from `--max-memory-mb`, which defaults to `0` — and `0`
  disables that dimension. The concurrency cap was live; the memory ceiling was
  not, unless an operator passed the flag by hand. `SandboxPolicy`'s
  `max_total_memory_percent` (default 50) existed and nothing read it. The
  daemon now measures host physical memory at startup (`sysctl hw.memsize` on
  macOS, `/proc/meminfo` on Linux), resolves the policy percentage against it,
  and states the ceiling in force on its log; `--max-memory-mb` remains as an
  explicit operator override. An unmeasurable host fails closed to a
  conservative 4096 MB — never to "unlimited", and never by refusing to boot.
  This mattered more after MGIT-95 made per-sandbox memory declarable up to
  `max_memory_mb` (16384): eight sandboxes could legally ask for 128 GB with
  nothing to refuse them. The per-sandbox bound is a *per-launch* bound; this is
  the *fleet* bound, and the two refusals stay deliberately distinguishable.
  (MGIT-98, FR-17.26, SEC-09)

- **On a host too small for the memory policy in force, mgit now says so.**
  Where the resolved ceiling lands below one default-sized sandbox, every launch
  is refused; the refusal used to advise freeing capacity or waiting, which can
  never help. It now reports that the launch "cannot be admitted even on a
  completely idle host" and names `max_total_memory_percent`, and the daemon
  warns about it at startup rather than leaving it to be discovered at the first
  failed launch. The ceiling is not quietly raised to fit — silently
  oversubscribing a host the operator sized on purpose is worse than an
  explanation. (MGIT-98)

- **The aggregate ceiling counted the wrong default.** A sandbox that declared
  no memory was accounted at a hardcoded 2048 MB fallback rather than at the
  host policy default it actually receives, so under any policy with a different
  `memory_mb` the ceiling counted something other than what the host was handing
  out. (MGIT-98)

### Added

- **A sandbox's CPU/memory/disk are now declarable at launch and bounded by host
  policy.** `mgit sandbox launch` and `mgit work` take `--cpus`, `--memory-mb`
  and `--disk-quota-mb`; unset still takes the host policy default. The fields
  existed in the API and reached the VM config already — nothing could set them,
  so a workload larger than the 2048 MB default had no route except editing the
  operator's host policy. `SandboxPolicy` gains `max_cpus` / `max_memory_mb` /
  `max_disk_quota_mb`, and a request above one of them is **refused naming the
  limit and the policy field that set it — never clamped**. Clamping would
  reproduce the defect one level up: a caller that asks for 4 GB, silently gets
  2, and concludes memory was already ruled out. The fleet-wide FR-17.26 ceiling
  still applies on top and now reads differently ("the host is already running N
  sandboxes") so "this launch is too big" and "the host is full" are never
  confused. (MGIT-95, R-H212)

- **The ceiling is visible, because its invisibility is what caused harm.** A
  customer's production build peaked at 2.10 GB against a ~1.94 GB guest, died
  with exit 134, and the agent — reasoning correctly from all it could see —
  rewrote the production bundler config to fit. The effective caps are now
  reported by `mgit sandbox status`, stated in the generated CLAUDE.md block
  along with an explicit instruction not to reshape the project to fit the
  guest, and printed by `mgit run` / `mgit sandbox exec` when a command dies by
  a signal or the guest stops answering mid-command. (MGIT-95)

- **We do not claim to detect guest OOM, and `docs/adr/014` records why with
  evidence.** The customer's failure is V8 aborting itself against a heap it
  sized from guest RAM — the kernel never runs its OOM killer, so there is
  nothing to detect. And when the kernel *does* fire (reproduced live on
  libkrun) it kills the guest supervisor that would have reported it: the host
  sees a dropped exec channel and a refused vsock dial, never an exit code. So
  mgit reports what it knows for certain — the cap in force — and leaves the
  conclusion to the caller. (MGIT-95)


## [0.4.5] - 2026-08-12

The Linux release. `-tags libkrun` on Linux went from "never validated" to a
continuously gated path in one sequence, and the four defects that stood between
those two states were each measured before they were fixed.

**Why it matters to a deployment:** before this, a Linux agent loop that edits
the host worktree between rounds had no validated option — firecracker cannot
re-stage into a running guest or export from it (it delivers an ext4 image built
at launch), and Linux libkrun was unproven. Now it has one.

**Backend matrix — three validated columns, continuously gated:**

| | macOS / libkrun | Linux / firecracker | Linux / libkrun |
|---|---|---|---|
| launch, exec, land | live (manual per release) | live in CI | **live in CI** |
| live egress policy (grant → revoke) | live | live in CI | **live in CI** |
| host edits → running guest (`sandbox sync`) | live | refused by design | **live in CI** |
| artifact export (`sandbox export`) | live | refused by design | **live in CI** |

firecracker's two refusals are deliberate and fail closed naming the backend;
they are a property of delivering the worktree as a launch-time image, not a
gap in its implementation.

### Known limitations (0.4.5)

- **On Linux/libkrun the guest root is writable only under `/tmp`, `/etc` and
  the worktree.** An agent can build, test and commit; it cannot `apt install`
  across the image root. The cause is upstream — libkrun's Linux virtio-fs
  answers `FS_IOC_GETFLAGS` with `EOPNOTSUPP`, which overlayfs propagates, so
  copy-up fails — and lifting it needs that errno fixed, not a change here.
- **A host write landing in the same second as a previous guest read, at
  unchanged length, can be missed on both libkrun backends** (cached size and
  1-second mtime granularity both look unchanged). No sync test is that tightly
  spaced; a fast agent loop could be. Tracked as MGIT-93.
- **`mgit sandbox shell` is unavailable in every build** — the interactive
  vsock-PTY transport is not implemented on any platform. `mgit run` and
  `mgit sandbox exec` cover non-interactive use, which is what an agent loop
  needs. Tracked as MGIT-94.
- **Per-sandbox resource caps are not yet declarable at launch.** The guest
  takes the policy default (2 vCPU / 2048 MB); a workload needing more cannot
  ask for it, and a build that exceeds the cap fails with the guest's own
  opaque OOM rather than a message naming the limit. Tracked as MGIT-95.
- **`sandbox sync` and `sandbox export` remain refused on firecracker**, as
  above. A loop that needs them should run libkrun on either platform.

### Fixed — a launch whose guest never starts now fails, loudly, instead of reporting success

**`mgit sandbox launch` and `mgit work --sandbox` reported a working sandbox
whose guest had already died.** Launch waited for the VMM and nothing else, so
the operator was told containment was established and the first command later
failed with a socket path. That is how MGIT-89 hid a completely broken backend
for weeks: every networked sandbox on Linux/libkrun was dead on arrival and the
launch said nothing. (MGIT-92)

A boot now CONFIRMS the guest is serving before it counts as a launch, and a
guest that never answers is torn down and reported with **the tail of its own
console** — the place its startup error was already being written:

```
mgit run: sandbox exec: sandbox ensure-running: libkrun launch: guest never
answered on its control channel within 15s: ...
guest console (tail):
{"msg":"libkrun vm entering","event":"krun_vm_enter",...}
mgit-guest: write /etc/resolv.conf: operation not supported
```

That last line is MGIT-89's actual cause, now delivered at the moment of
failure instead of after a week of investigation.

- **It costs the healthy path nothing.** Measured on real KVM, first exec after
  a launch: 5679/5761/5741 ms before, 5755/5737/5746 ms after — identical
  within noise, because the wait already existed inside the first exec and has
  only moved earlier, to where the diagnosis is possible. The 15s bound is paid
  only by a launch that was already broken, and a sandbox nobody can use is
  worth far more than 15s to an agent that would otherwise walk into it.
- **Which step fails closed: the BOOT, not the registration.** Provisioning
  stays lazy (FR-17.9/17.10) — `work --sandbox` registers and the microVM starts
  on first use — so confirming at registration would mean booting eagerly and
  changing the lifecycle contract. What did change is the message: registering
  now says **"Registered sandbox … (created; the microVM boots on first use, and
  that boot fails closed if its guest does not come up)"** instead of
  "Launched", which was the word that invited the wrong reading.
- **All four backends, each confirming what it can actually prove.** The wait
  lives once in the shared microVM manager (firecracker, libkrun, vzf, hyperv)
  and asks the guest to answer on its control channel; the container fallback
  has no guest and no vsock, so it confirms the container is RUNNING and quotes
  `podman logs` on failure. A backend with no exec transport wired is skipped
  rather than failed, because there is no control plane there to confirm.
- **MGIT-91's first-command retry is untouched and still armed.** The readiness
  probe deliberately does NOT mark the guest as having answered: it is answered
  by a lookup failure on the read path, which is weaker evidence than a real
  command round trip, so the retry window stays open for the caller's first real
  command. A test pins exactly that.

### Fixed — the first command after a launch no longer dies on a connection reset

**`mgit run` failed on the first command after every sandbox launch on
Linux/libkrun**, with `connection reset by peer` on the guest's vsock exec
socket, and the same reset hit the library path roughly one run in ten. Both
are gone: the shared posture pass (launch -> exec -> land) now PASSES on
Linux/libkrun, and it is a required CI gate again rather than a tripwire.
(MGIT-91)

**Two causes, one product and one test — and the guest was innocent of both.**
Reading the per-VM console right after a failing exec showed mgit-guest logging
*nothing at all* for that attempt and serving the next one normally: no panic,
no crash, the guest simply never received it.

- **The product bug: the first-command retry could not fire.** `microvm.Manager`
  already retries a first command that never reaches a listener, but its
  predicate matched only `io.EOF`. libkrun creates the host-side vsock socket
  when the VM is configured, so an exec issued before mgit-guest binds its
  listener CONNECTS successfully and is then reset by the VMM — `ECONNRESET`,
  never `io.EOF`. The predicate now also matches `ECONNRESET` and `EPIPE` (the
  same event seen from the writing side). **All three of the caller's guards are
  unchanged**: a retry still happens only while the guest has never answered,
  only with no output whatsoever, and only inside the readiness deadline — so a
  reset that kills a long-running build mid-stream still surfaces immediately
  instead of being silently re-run. A test pins exactly that.
- **The test bug: one e2e waited on the wrong marker.** `MgitGuestControlPlane`
  waited for the console to mention `mgit-guest`, which is its very first log
  line — printed before it binds. Every sibling test that drives the exec port
  waits for `"vsock_port":1024`; this one was the outlier, and it dialed
  straight into the unbound window.

**Verified by repetition, not by one green run** — the standard this repo's own
FLAKY category exists to enforce: 20/20 consecutive runs of the previously
intermittent test, 14/14 first-`mgit run`s after launch (it was 0/3 before), two
consecutive clean passes of the full real-VM battery on Linux/KVM, and the
macOS suite twice with no regression.

- **The CI gate's INTERMITTENT list is now empty**, alongside its known-gap
  list, and the validated column stands at 25. Both lists are kept, empty, so
  the next finding has an obvious home — and both steps now no-op cleanly on an
  empty list rather than running a pattern that matches everything.

### Fixed — a deleted file can no longer be read by a running guest

**`mgit sandbox sync` reported a path deleted while a guest process could still
read its old contents** on Linux/libkrun. That is the silent-staleness failure
this verb exists to prevent: an agent that removes a file and re-runs its tests
would test the removed file and believe the result. (MGIT-90)

**What was actually wrong was narrower than the report, and the measurement is
the useful part.** Creates were never broken — a host-created file is visible to
the guest immediately, even on a name the guest had already looked up and found
absent. Deletes were, and not because the write failed: the guest's own
directory listing was correct the instant the host unlinked. What lingered was
the guest kernel's cached NAME lookup, which on libkrun's Linux virtio-fs
survives ~5 seconds:

```
host unlink -> guest ls        : gone immediately
host unlink -> guest stat/read : still resolves, returns OLD CONTENT
                                 ...for 5.01s, then vanishes on its own
same measurement on macOS      : 0.00s — which is why this never surfaced there
```

- **The sync now empties a file before unlinking it.** Truncation IS observed
  immediately on both platforms, so a guest holding the stale name finds an
  empty file rather than the deleted bytes — a build that reads it fails loudly
  instead of silently succeeding against code you removed. One extra syscall,
  unconditional, and invisible on backends with no entry cache.
- **The e2e now asks the GUEST, twice.** It asserts the deleted path is absent
  from the guest's own directory listing and that nothing can be read from it —
  the host's view of the share was never in doubt and was never the bug. A new
  in-guest `fsprobe` fixture exists because these minimal guest bases carry no
  shell to ask with.
- **The residual is documented rather than papered over:** the NAME may resolve
  for a few seconds after a delete on Linux/libkrun. The timeout belongs to
  libkrun's filesystem server; nothing in mgit or in the guest mount can shorten
  it, and libkrun's virtiofs API exposes no cache knob (only DAX window,
  read-only and permission semantics).
- **With this, every measured Linux/libkrun gap is closed.** The CI gate's
  known-gap list is empty for the first time and its validated column stands at
  24 capabilities; the tripwire machinery is kept, with its list empty, so the
  next gap has an obvious home.

### Fixed — a networked sandbox starts on Linux/libkrun, and the reason it did not is one errno

**Every `--network allowlist` and `--network open` sandbox died at startup on
Linux/libkrun.** mgit-guest writes the resolver into `/etc/resolv.conf` while
configuring the guest NIC, and that write failed with `operation not
supported` — so the guest exited before serving and the live `sandbox policy`
verbs had nothing to act on. All nine network and live-policy real-VM tests now
pass there, including the MGIT-72 kill/drain pair on an established flow, and
they have moved from the CI gate's known-gap list into its validated column.
(MGIT-89)

**The cause was not what any of the standing theories predicted, and the
measurement is worth keeping.** The overlay upper is tmpfs and takes
`trusted.*` xattrs; the scratch mount happens exactly as intended; the lower
reads fine. What fails is overlayfs's COPY-UP, and only copy-up:

```
create /newdir/x         (upper only)   -> ok
create /mgit-probe       (root, upper)  -> ok
create /etc/probe        (lower dir)    -> operation not supported
chmod  /etc              (lower dir)    -> operation not supported
open   /bin/cat O_WRONLY (lower file)   -> operation not supported
```

overlayfs calls `ovl_copy_fileattr()` on every regular-file and directory
copy-up, which issues `FS_IOC_GETFLAGS` on the lower inode. It tolerates
`ENOTTY` and `EINVAL` as "this filesystem has no file attributes" and
propagates anything else. Measured on the same guest kernel (libkrunfw
6.12.91), same guest, same overlay options:

```
libkrun macOS fs device : FS_IOC_GETFLAGS -> ENOTTY (25)      copy-up works
libkrun Linux virtio-fs : FS_IOC_GETFLAGS -> EOPNOTSUPP (95)  copy-up fails
```

That is the entire macOS/Linux asymmetry. No overlay option set changes it —
`userxattr`, `index=off`, `metacopy=off`, `redirect_dir=off` and `xino=off`
were each mounted and re-tested.

- **The repair is a capability probe, not a platform check.** mgit-guest tries
  to create a file in `/etc`; only if that fails with the copy-up refusal does
  it snapshot the directory, mount a tmpfs over it and restore the snapshot —
  so the guest keeps the image's resolver config, CA bundle, `passwd` and
  `nsswitch` rather than getting an empty `/etc`. firecracker, vzf and
  macOS/libkrun take the no-op branch, and the day libkrun's virtio-fs answers
  `ENOTTY` the probe passes and the workaround stops running by itself.
- **What is still not writable, stated plainly:** everything under `/` except
  `/tmp`, `/etc` and the mounted worktree. An agent can build and commit; it
  cannot `apt install`. Lifting that needs the upstream errno fixed, not
  another workaround here, and the capability tables say so.

### Linux libkrun is a validated path now — with two measured limits stated up front

**The standing limitation carried since 0.4.2 — "Linux libkrun (`-tags libkrun`)
is still not a validated path" — is retired.** A libkrun microVM boots on real
Linux/KVM, and boot, guest exec over vsock, `sandbox sync` of file content,
`sandbox export` and the SEC-03 hostile-guest battery all hold there. It runs as a **CI gate** (`sandbox-live-linux-libkrun`
in `e2e.yml`) on every push, PR and release, alongside the firecracker one —
not as a one-time report. (MGIT-87)

The 2026-07-29 finding that the boot **hung at VM entry** does not reproduce,
and the cause was not Linux: the build recipe cloned libkrun's `main`, and
upstream after v1.19.4 replaced `krun_set_workdir` with a stub returning
`-ENOTSUP`, so VM configuration failed before the guest ever ran. libkrun
v1.19.4 and libkrunfw v5.5.0 — the pair Homebrew ships, so the same pair macOS
is validated against — are now pinned in `scripts/sandbox-image/pins.env` and
built by `scripts/sandbox-image/build-libkrun.sh`, which CI and a developer run
identically.

**Three things do NOT carry over from macOS.** All were measured on real KVM
and reproduced in more than one environment (a hosted runner inside a container
with `/dev/kvm` passed through, a hosted runner on the bare host as an
unprivileged user, and a bare-metal KVM host), so none is an artifact of one
setup:

- **The guest exec channel resets**, and `mgit sandbox land` sits behind it, so
  neither is claimed. Every failure carries the same signature: "connection
  reset by peer" on the guest's vsock exec socket. Through `mgit run` it failed
  on every attempt — on bare metal only for the everyday
  `mgit run -- echo hi` while `/bin/echo` worked, on a hosted runner for the
  absolute path as well — and through the library path it passed in three
  environments and then failed in a fourth run with no code change. The VM
  survives either way. Intermittent, in other words, rather than simply broken,
  which is why the affected tests are RUN and reported by CI but gated neither
  way: asserting a pass would make the gate flaky and asserting a failure would
  entrench a defect that usually does not fire. Tracked as MGIT-91; it is also
  why the shared posture script cannot pass on this backend.

- **The guest's root filesystem is effectively read-only, and a guest with a
  network therefore does not start at all.** Creating a file under the
  writable-root overlay fails with `operation not supported`; `/tmp` and the
  mounted worktree are writable and behave normally (measured through the
  production path on a real `debian:12` base, not only in a test fixture). An
  agent can build and commit in its worktree but cannot install packages or
  write `/etc`. mgit-guest writes the resolver into `/etc/resolv.conf` during
  startup, so it dies on that same refusal: every `--network allowlist` and
  `--network open` sandbox exits before it serves, and the live `sandbox
  policy` verbs have no running enforcer to act on. The daemon does select the
  correct enforcer for them (`policy_wired backend=libkrun`, with the Linux
  daemon-side runner correctly not winning) — there is simply nothing live to
  address. `--network none` is unaffected, and is the mode the validated
  column runs. Tracked as MGIT-89.
- **`sandbox sync` carries content, not the namespace.** A host edit to an
  existing file reaches the running guest, asserted through the guest itself.
  A file the host CREATES or DELETES does not: the verb reports the delete as
  applied and the guest keeps reading the old file. That is the silent-staleness
  class this feature's e2e exists to catch, and it is why the tables say
  "content edits only" rather than "yes". Tracked as MGIT-90.

**The consequence for Linux deployments, stated plainly:** a loop that needs
guest egress wants firecracker; a loop that needs host edits delivered into a
long-lived guest, or artifacts read back out, wants libkrun and must run
offline. A loop needing both is not served on Linux today — MGIT-86 remains
open, and this result is what decides it is still needed rather than moot.

- **The gate cannot pass by skipping, and cannot silently understate the
  backend either.** `scripts/e2e/libkrun_linux_column.sh` holds the capability
  column as data: it asserts every validated test PASSED by name, that a SKIP
  anywhere in that run is a failure, that the two known-gap tripwires still
  FAIL — a gap that closes turns the gate red until the tables here, in
  README.md and in docs/INSTALL-SANDBOX.md are updated — and that every
  `TestE2E_Libkrun_RealVM_*` in the package is classified in one of those
  lists, so a new test cannot be added ungated.
- **Hosted runners can boot microVMs from inside a job `container:`** when the
  device is passed through (`options: --device /dev/kvm`). Measured: the device
  is present, `KVM_CREATE_VM` succeeds, and no udev rule is involved because a
  job container runs as root — MGIT-78's host-side rule cannot help a container,
  since host steps cannot run before the job's own container starts.

### Added — the remaining `sandbox sync` collision classes, on real hardware

- **All three refusal classes in ADR-011's collision policy now have a real-VM
  e2e**, not just the both-sides-changed one: a host ADD over a file the guest
  created (refused as `created in the guest`, then delivered by `--force` with
  the destroyed path reported), and a host DELETE of a path the guest changed
  (refused, then applied once the guest's copy matches what was delivered).
  Each pairs its refusal with a positive control on the same sandbox, because a
  sync that is simply broken also refuses everything. The delete case is what
  found the Linux namespace gap above. (MGIT-87, MGIT-76)
- **The artifact-export e2e no longer asserts a macOS implementation detail.**
  It required the provenance sidecar to say `"mode_source": "share-record"`,
  which is true only where libkrun presents placeholder permission bits and
  records the real mode in a share attribute. On Linux the guest's mode is in
  the file's own bits, so a correct export was failing the test. It now asserts
  the exported mode AND that the attribution matches how the mode was actually
  observable — a stronger check on both platforms. (MGIT-87, MGIT-81)

### Fixed

- **The libkrun loader-path search missed `/usr/local/lib64`**, which is exactly
  where a from-source `make PREFIX=/usr/local install` puts libkrunfw on 64-bit
  Linux — and Ubuntu's `ld.so.conf` does not cover it either. Every Linux
  install is from source (no distro package, no tap), so without it a VM child
  can fail to find libkrunfw unless the operator exported `LD_LIBRARY_PATH`
  themselves. (MGIT-87)
- **`scripts/e2e/sandbox_posture.sh` branched on the operating system**, which
  silently equated "Linux" with "firecracker": a working Linux/libkrun daemon
  was sent down the kernel+rootfs branch and skipped for want of a kernel it
  does not use. It now dispatches on the guest input it was given — kernel +
  rootfs, or an OCI ref — the same shape of fix the macOS half needed in 0.4.3.
  (MGIT-87)

## [0.4.4] - 2026-08-11

An install and operability release: no change to how agents behave at runtime,
so it is a safe upgrade from 0.4.3 for anything already running. What it fixes
is everything *around* the binaries — how you get them, whether they will run
once you do, and whether you can tell which build you have.

**Backend capability matrix, stated plainly** because it decides what a
deployment can rely on and it is not symmetric:

| | macOS / libkrun | Linux / firecracker |
|---|---|---|
| launch, exec, land | live-validated | live-validated (CI-gated since 0.4.3) |
| live egress policy (grant → revoke) | hardware-proven | hardware-proven |
| host edits reach a **running** guest (`sandbox sync`) | yes | **refused** — launch-time ext4 image |
| artifact export (`sandbox export`) | yes | **refused** — same reason |

Both refusals are deliberate and fail closed with the backend named; neither is
a regression. On firecracker every exec after launch runs against the
launch-time copy, so a loop that edits the host worktree between rounds needs
the libkrun/vzf backend today. Closing that gap is tracked as MGIT-86.

### Added — the release smoke is a script, and runs on every publish

- **`scripts/e2e/release_smoke.sh` replaces the hand-executed post-publish checklist steps**, and a `release-smoke` job runs it on a fresh macOS runner after every release — a runner that did not build the artifact, which is the condition the manual step always asked for. It checks the published archive's contents, the Gatekeeper quarantine behaviour in both directions, that the shipped binaries run, that `mgit` and `mgit-sandboxd` report the same build, libkrun's `NET=1` capability, and that the Homebrew tap is reachable **unauthenticated** — the check that would have caught MGIT-66, and which no authenticated tester could ever fail. (MGIT-84, MGIT-66)
- **It probes capability instead of consulting a version table**, because a maintained list of "which release has what" is one more thing that drifts — which is the defect the ticket was filed about. A skip is reported loudly and never reads as a pass.

### Added — `install.sh`, and it is now the headline install, and it is now the headline install

- **`curl -fsSL .../install.sh | sh`** resolves the latest release, verifies the archive against the published `checksums.txt` **before installing anything**, and lays `mgit`, `mgit-sandboxd` and the guest pair out where mgit looks for them (`$PREFIX/bin` + `$PREFIX/libexec/guest`, so `mgit-guest` never lands on PATH). `MGIT_VERSION` pins a release; `MGIT_PREFIX` chooses where it goes. No sudo, ever — it picks `/usr/local` only when already writable, else `~/.local`.
- **It fixes the macOS quarantine problem for real, without notarization.** `com.apple.quarantine` is written by the *downloading app on the user's machine*, so nothing done at build time can remove it — but only quarantine-aware apps (browsers, AirDrop, Mail) set it. curl does not. Measured: a browser-downloaded `.tar.gz` yields quarantined binaries even through command-line `tar`, and Gatekeeper SIGKILLs them; the same archive fetched by curl installs and runs clean. The README now states which channels are affected and which are not, instead of only offering the `xattr -d` remedy. (MGIT-64)

### Added — `mgit-sandboxd --version`

- **The daemon can now say which build it is.** It shipped in every archive and installed beside `mgit` via Homebrew, but had no `--version` flag at all: it answered `flag provided but not defined: -version` and exited 2. An operator debugging a sandbox that will not launch could not ask the binary what it was. (MGIT-83)
- **It answers before touching the host** — no socket bound, no host root read, no backend probed — because the question is asked precisely when the daemon *cannot* start. The version goes to stdout, where a caller can capture it, rather than joining the structured log on stderr.
- **Both binaries now report one build from one implementation.** Version resolution and formatting moved into `internal/buildinfo`, and the `-X` ldflags in the Makefile and GoReleaser target that package for `mgit` *and* both `mgit-sandboxd` builds — the daemon's builds previously carried no version stamp at all, so adding the flag alone would have made it report `dev` in every release. A guard test asserts the ldflags path in both build configs matches the package's real import path: moving the package would otherwise still compile, still link, and silently ship binaries reporting `dev (commit: none, built: unknown)`.
- **This was found by running the release checklist rather than reading it.** The Gatekeeper smoke step chained `./mgit --version && ./mgit-sandboxd --version` to distinguish "the binary ran" from "Gatekeeper SIGKILLed it" — so the missing flag's exit 2 read as exactly the failure the step exists to catch. The step now also compares the two strings, since they are stamped together and a mismatch means the archive was assembled from two different builds. (MGIT-83, MGIT-64)

## [0.4.3] - 2026-08-10

Three verbs the integrating lane asked for, two defects found by using mgit on
mgit, and the Linux live gate that had been manual since the sandbox shipped.
The commit defect is the one to read first: `mgit commit` reported success for
work it never recorded, which is the failure mode this whole substrate exists
to prevent.

**Live validation — both GA platforms, on real hardware.**

- **Linux / firecracker (KVM):** the posture pass (launch → exec inside the
  guest → land round-trip) plus the full `TestE2E_*` battery — exec/land,
  hostile-guest (SEC-03) ×3, notify auto-land, overlay-root writability,
  provenance, the guest-resolver egress allow/deny pair with real bytes, NAT,
  port publishing, and the live-policy kill/drain pair — all pass, none
  skipped. This now runs **in CI on every release** rather than on a hand-held
  box (MGIT-78, below).
- **macOS / libkrun (Apple Silicon):** the full real-VM e2e suite plus the
  release posture pass, composing a guest base from `debian:12` → launching a
  task sandbox → exec inside it → land round-trip. Still a manual pass; the
  hosted-runner fleet has no Apple Silicon virtualization.

Standing gaps are listed under Known limitations — there is no undisclosed
"validated" claim in this release.

### Known limitations (0.4.3)

- **`mgit sandbox sync` (MGIT-76) is live-validated on libkrun only.** It is
  refused, by design and with the backend named, on firecracker — that backend
  delivers the worktree as a launch-time ext4 image the host cannot write into
  — so there is no firecracker behaviour to validate beyond the refusal.
- **Artifact export (MGIT-73) ships for the virtiofs backends** (libkrun, vzf).
  firecracker fails closed naming the same limitation. The guest-mediated
  stream that would lift it is not built.
- **Exported directories are created `0750`** whatever mode the guest set —
  the one exported mode that is not a mode someone observed. Called out in
  `mgit sandbox export --help` rather than left implicit.
- **Linux libkrun (`-tags libkrun`) is still not a validated path.** It is not
  the Linux default; CI build+vets it so a compile regression is caught, which
  says nothing about whether it boots. Unchanged from 0.4.2.
- **The container fallback backend was not measured** for the file-mode work
  below; it degrades to a plain host stat, which is correct but unproven.

### Added — `mgit sandbox sync`, the verb ADR-011 already promised

- **The explicit worktree-sync verb now exists.** ADR-011 described host→guest worktree sync as running "automatically before an exec ... plus an explicit `mgit sandbox sync` for control." Only the automatic half shipped in 0.4.2; the verb did not exist, which the integrating lane found by reading the ADR and then `mgit sandbox --help`. Their workaround was a probing no-op `exec` every round to force a re-stage — which is this verb spelled awkwardly, at the cost of a guest process per round. `mgit sandbox sync --task <id>` now re-stages on demand and reports what it did: counts and paths per collision class. (MGIT-76)

- **`--dry-run` returns the collision classification without touching the guest.** This is the part that was unobtainable any other way: until now the only way to discover a conflict was to attempt work and be refused. A dry run runs the same staging build and the same collision policy a real sync runs — which is what makes the answer trustworthy — then stops before any write, leaving both the guest's tree and the delivery manifest exactly as they were. It reports conflicts as **data**, not as a refusal, and says plainly whether a real sync would be blocked. `--json` emits the whole report, conflicts included. (MGIT-76)

- **`--force` carries the pre-exec semantics unchanged**: it overwrites paths the guest changed since delivery, and every destroyed path is reported and audited. `--dry-run --force` answers the question worth asking *before* forcing — exactly which un-landed guest paths would be destroyed. (MGIT-76)

- **An unchanged worktree is a genuine no-op and says so** ("already up to date"), established by the cheap delivery-manifest comparison rather than by re-staging blindly, so polling the verb between rounds costs nothing and never reports phantom work. (MGIT-76)

- **MCP parity: `mgit_sandbox_sync`.** An agent is a first-class caller of this verb — it is the one sandbox operation an agent needs *between* rounds. It is the deliberate exception to sandbox-lifecycle-is-CLI-only: it grants no new authority (the daemon re-stages through the same host-side invariants a launch enforces), and its `dry_run` form is the only way for an agent to learn which paths diverged without running a command in the guest. A refusal comes back as a tool **error** carrying every conflicting path, never as a result an agent could read as a completed sync. `docs/MCP-PARITY.md` records the decision and the remaining, still-deliberate CLI-only lifecycle gap. (MGIT-76)

### Fail-closed, not silently — the property this verb is most at risk of losing

- **On a backend that delivers the worktree as a launch-time image, sync refuses and names the limitation.** firecracker packs the worktree into an ext4 image at launch which the guest then mounts; the host cannot write into it without corrupting it. All three forms (`sync`, `--dry-run`, `--force`) exit non-zero naming the backend and pointing at re-launch as the remedy. It must not no-op and report success: **a sync that claims to have run is how stale code gets executed.** (MGIT-76)

- **It is a caller, not a second mechanism.** The verb, the automatic pre-exec stage and a launch all run the same `internal/sandboxd/staging` build and the same collision policy, so a sync can never deliver something either of the others would have refused — the single-path property that is the whole security argument for staging over a live mount (SEC-03). The host-wide resource-ceiling decorator forwards the capability explicitly: an optional capability a decorator silently drops would have made every backend look unsyncable. (MGIT-76)

- **A dry run is never recorded as a sync.** An audit trail that cannot distinguish a query from a delivery is worse than none — a reviewer reconstructing "what code did this sandbox execute" would be reading events that never happened. (MGIT-76)

- **Validated on a real libkrun microVM on Apple Silicon**, not only in unit tests, and **through the production launch path** — `microvm.Manager.Launch` → `Manager.SyncWorktree`, the entry the CLI verb itself reaches — rather than against the hypervisor beneath it. Proven live: the guest reads the delivered content through a live virtiofs mount; the unchanged worktree reports a no-op; `--dry-run` classifies a real change while the guest still reads the old bytes; a conflict is refused naming the path; and — the assertion that makes the others readable — a **positive control on the same running VM**, the same path delivering once the divergence is resolved, so the delivery is attributable to the verb and a refusal is distinguishable from a broken path. (MGIT-76)

- **Two constraints the production path enforces, now covered rather than assumed.** A sandbox launched against a repository whose root *is* the worktree fails closed (`ErrSharedStoreReachable`): SEC-03 requires the shared object store to live outside the mounted worktree, which is why `mgit work`'s linked-worktree layout is the supported one. And the guest root is shared **read-only** (FR-17.17), so a guest base missing the standard FHS directories cannot complete `mgit-guest`'s writable-root overlay and dies before its first log line — a base composed with `mgit sandbox base from <oci-image>` always has them. (MGIT-76)

### Added — live egress policy: grant, then revoke, without destroying the sandbox

- **`mgit sandbox policy set|revoke|show --task <id>` changes a RUNNING sandbox's egress allowlist.** The sequence this exists for is one an agent runs unattended: grant package-registry egress, `npm install`, revoke, then run the untrusted build and tests with the network closed. Until now the only revoke was a relaunch — which destroys the environment the install just provisioned — so callers held egress open for the whole run and disclosed it, a weaker posture than their design intended. The authorizer is consulted per connection, so a mutated allowlist takes effect on the next flow with no VM involvement. (MGIT-72)

- **Revoke TERMINATES established connections; `--drain` is the named opt-in to let them finish.** This is the opposite of firewall convention and it is deliberate: a draining connection is the exfiltration channel you just revoked, and a hostile guest chooses how long it lives. The choice is documented at every verb that can terminate a connection, in the MCP tool description, and in **ADR-012** — a caller who assumes the other behaviour would be exposed, so it is stated where the caller is, not only in the ADR. (MGIT-72)

- **An empty `set` is refused rather than silently revoking everything**, and `show` reports the policy **in force**, which after a mutation is not the launch-time policy `mgit sandbox status` shows. Mutations are atomic — a flow is authorized against the old policy or the new one, never a mixture — and every one is written to the append-only audit trail with the task binding, the resulting policy, and how many established flows it terminated. (MGIT-72)

- **Only the host may mutate policy (SEC-05).** No control-plane route exposes this to the guest side, and a guest-side attempt is refused; there is a test that asserts it rather than a comment that claims it. (MGIT-72)

- **Proven on a real libkrun microVM, on the assertion that actually carries the guarantee.** The first live run reported `killed=0` because the probe closed its connection between calls — which cannot distinguish "nothing to kill" from "kill is broken". The probe now HOLDS a connection open across the revoke: the held flow **dies** (`killed=1`, guest-observed EOF) by default and **survives** a `--drain` revoke, and in both cases a fresh dial is denied afterwards. The drain half is what makes the kill a decision rather than a coincidence. (MGIT-72)

- **firecracker is implemented and reviewed, but NOT proven on hardware.** Its transparent proxy splices through the same flow registry the revoke closes, and its runner fails closed on a sandbox with no egress stack rather than reporting a revoke it did not perform — but the two backends enforce by different mechanisms, so the libkrun pass is not evidence for it. Its e2e exists and needs a KVM host. Tracked as **MGIT-78**; ADR-012 says so in as many words. (MGIT-72)

### Added — `mgit sandbox export`: bring guest-built artifacts out, without weakening the airlock

- **Guest-built artifacts had no way home.** The guest works on a *staged copy*, so a `node_modules` tree or build cache it produces dies with the sandbox — every round re-did work the previous round had already done, which is exactly the once-per-lockfile reuse promise a provisioning cache depends on. `mgit sandbox export --task <id> <guest-path> <host-path>` (and the `mgit_sandbox_export` MCP tool, because the caller is usually an agent) copies a named path out to a named host destination. (MGIT-73)
- **The host names both ends, and the guest does not participate at all.** On the virtiofs backends (libkrun, vzf) the guest's worktree *is* a host directory, so an export is a host-side READ of the staged tree — no control-plane hop, no guest cooperation, nothing the guest can observe or interpose on. A guest-chosen destination would be a host-filesystem write primitive; there is no code path that accepts one.
- **Everything is rejected host-side, before any byte is written**, reusing `internal/sandboxd/staging`'s symlink-escape check (now shared by both directions rather than reimplemented) plus: no absolute or traversing guest paths, no symlink or hardlink leaving the exported subtree, no irregular files, and **never the sandbox's private `.mgit` store** — committed objects still cross only through the verified land airlock.
- **Collisions are refused, never overwritten**, and the transfer is bounded (4 GiB / 200,000 entries by default) so an export cannot fill the host disk. A refusal leaves the host filesystem exactly as it was — the artifact is built in a temporary directory beside the destination and renamed into place.
- **Every exported artifact carries provenance**: a `<host-path>.mgit-export.json` sidecar naming the sandbox, task, pinned base-image digest, per-file SHA-256s and a tree hash (the MGIT-61.15 attestation pattern applied to files), plus an `artifact_exported` record in the append-only audit trail with task binding, paths and byte count. An export that cannot be audited is undone.
- **`land is the only bridge` still holds, and is restated rather than deleted.** The hostile-guest tests assert something precise: no guest activity changes the host's *shared git store* without land. Export is a second, narrower bridge — for files, into a host-named destination — and it never touches the store. Its own hostile-guest coverage runs on a **real libkrun microVM**: a real guest builds a tree and plants escapes through virtio-fs, the host exports the good tree intact and refuses the escapes, the private store and a colliding destination.
- **Known limitations.** firecracker fails **closed** (`ErrArtifactExportUnsupported`): it delivers the worktree as a launch-time ext4 image the guest has mounted, so there is no host directory to read from, and the guest-mediated stream that would be needed is not shipped in v1 — the same call MGIT-71 made for sync. Separately, measured on macOS/libkrun: virtio-fs presents guest-created files to the host with its own permission mapping (a guest's `0755` reads as `0600`), so an exported tree reproduces the modes the **host observes on the share**, not the modes the guest set.

### Added — the Linux live sandbox gate runs in CI, so it can no longer be skipped

- **The Linux/firecracker live sandbox pass is now a CI gate, not a manual
  step (MGIT-78).** `e2e.yml` gained `sandbox-live-linux`: it builds `mgit`
  *and* `mgit-sandboxd` (no `-tags libkrun`, so the daemon links firecracker),
  installs a sha256-pinned firecracker VMM, builds the pinned guest kernel and
  the reproducible rootfs, then runs `scripts/e2e/sandbox_posture.sh` and the
  whole `internal/sandboxd/backend/firecracker` `TestE2E_*` battery — once
  unprivileged, then again under `sudo -E` for the tap/iptables half. Because
  `release.yml` already gates on `e2e.yml`, this gates every release.
  - The premise it replaces was simply stale: the checklist and the old job
    both asserted that GitHub-hosted runners have no nested virtualization.
    Measured on 2026-08-10, `ubuntu-latest`, `ubuntu-24.04` and `ubuntu-22.04`
    all expose `/dev/kvm` (`KVM_CREATE_VM` succeeds once the `static_node=kvm`
    udev rule grants the runner access), and real firecracker microVMs boot
    there. The old job never even built `mgit-sandboxd`, so it could only ever
    run the SKIP branch.
  - The gate refuses to pass on a skip: it asserts `/dev/kvm` is usable, greps
    for the literal `SANDBOX POSTURE E2E: PASS (live)`, fails on any `--- SKIP`
    in the privileged run, and names the two MGIT-78 live-policy tests
    explicitly.
- `scripts/sandbox-image/fetch-firecracker.sh` — fetch and sha256-verify the
  pinned firecracker VMM, mirroring `fetch-kernel-fc.sh`. Nothing in the repo
  installed the VMM before; the Linux pass assumed a hand-provisioned host that
  already had it, which is part of why that gate could not run anywhere else.
- `FC_CMDLINE`/`VZ_CMDLINE` moved into `scripts/sandbox-image/pins.env` so the
  bundle builder, CI and a human following the release checklist register guest
  images with the same kernel command line. An image registered *without* one
  boots a kernel with no `root=` and no `init=`, and the only symptom is
  `guest vsock not ready within 15s`.
- `e2e.yml` accepts `workflow_dispatch`, so the live gate can be re-run on
  demand against any branch.

### Fixed — `mgit commit` reported success for work it never recorded

- **A commit with nothing staged printed a hash and exited 0, and its tree was byte-identical to its parent.** The agent received the success signal for a checkpoint that does not exist; only reading the `.mgit` store directly revealed it. `mgit commit --help` documented `--allow-empty` as "Allow commit with no staged changes" — a flag that only means something if the default refuses — but the flag was bound to a variable read *only* by the `--dry-run` printf, so it never reached the service, and the service never compared the tree it was about to write against its parent's. **`mgit commit` now refuses**, exits non-zero, and names the remedy (`mgit add <path>`, `mgit commit -a`, or `--allow-empty` for a deliberate empty commit). `--allow-empty` now does what its help always said. (MGIT-77)
- **The instructions mgit generates walked agents straight into it.** The `mgit work`-generated CLAUDE.md block said "Run `mgit commit -m …`" and never mentioned staging, so an agent following mgit's own instructions produced a branch of empty commits and a land patch with zero hunks. **`mgit commit` gains `-a`/`--all`** — stage every change, including new files, and commit in one step — and the generated block now prescribes it, states that mgit records only staged changes, and explains what the refusal means. A two-command-per-step loop is a loop an agent drops half of; `-a` removes the opportunity. (MGIT-77)
- **The same failure one layer down: the land step landed nothing, quietly.** `mgit squash --to-git` on such a branch emits a well-formed mbox with no diff hunks, and `git apply` accepts it happily. The export now **warns on stderr** when the patch carries no hunks (stdout stays a clean patch when piped), naming the task and how work gets recorded. (MGIT-77)
- **`mgit status` distinguished "will be committed" from "will be silently dropped" by column position alone** (`M   path` vs. `  M path`). The default output now groups paths under "Changes to be committed", "Changes not staged for commit" and "Untracked files", labels each entry in words, and says outright when nothing is staged. `--short`, `--porcelain` and `--json` are unchanged. (MGIT-77)
- **Behavioural note — this is a deliberate breaking change to a success path.** Anything that relied on `mgit commit` succeeding with an empty staging area (scripts, fixtures, an agent loop that never staged) now fails and must either stage, pass `-a`, or ask for `--allow-empty`. The REST surface returns **409 Conflict** instead of 201 for a no-op commit. The MCP `mgit_commit` tool, which exposes no separate staging tool, now **stages the working tree by default** (`stage_all`, default true) — without it every MCP commit could only ever have recorded an empty tree. (MGIT-77)

### Fixed — mgit's own generated scaffolding no longer lands in your repository

- **`mgit commit -a` no longer sweeps mgit's agent scaffolding into the task branch.** `mgit work` writes worktree-specific, host-specific files of its own — the generated `CLAUDE.md` block, `.claude/settings.json`, and under containment `AGENTS.md`, `.envrc` and the Cursor rule. None of it was ignored, so once MGIT-77 correctly made `mgit commit -a` *the* agent loop, the instruction mgit itself generates guaranteed that mgit's files reached the patch landed in the user's repository — where the content is not merely noise but wrong, describing one worktree's task binding and this host's sandbox availability. (MGIT-80)

- **The fix is in the tool, not in the instructions.** `mgit work` now records what it generated, per worktree, under `.mgit/generated` (already excluded from staging), and the bulk-staging walk behind `mgit add -A` / `mgit add .` / `mgit commit -a` skips those paths. Because the rule is recorded *provenance* rather than a filename blocklist, a project that tracks its own `CLAUDE.md` is unaffected — and both shapes of the bug fall out of one rule: scaffolding that is a brand-new untracked file, and scaffolding that is an **edit to a file the project already tracks**. Worktrees created before this change have no manifest and behave exactly as before. (MGIT-80, ADR-013)

- **A deliberate edit still commits: name the path.** `mgit add CLAUDE.md` stages it, and a later `mgit commit -a` does not retract that selection — a named pathspec is an unambiguous statement of intent, mirroring `git add -f` beating `.gitignore`. `mgit status` keeps telling the truth about the file being modified; mgit hides no real working-tree change from the reviewer. The generated block now says so, but the guarantee does not depend on an agent reading it. (MGIT-80)

### Fixed — exported artifacts keep the mode the guest set (macOS/libkrun)

- **An exported `node_modules` tree's `.bin` scripts arrived non-executable**, which blunts exactly the provisioning-cache reuse `mgit sandbox export` exists to enable. It was first recorded as a measured limitation of the share; measuring again, further in, showed it was recoverable. (MGIT-81)
- **What the measurement showed.** Inside a real libkrun microVM the guest's umask is `0022` and a control arm on a guest-private tmpfs kept every mode, so neither the guest nor the workload loses anything: libkrun's macOS filesystem device gives guest-created inodes *placeholder* permission bits (`0600` files, `0700` directories) and records the real `st_mode` in the `user.containers.override_stat` extended attribute (`0:0:0100755` for a guest `0755`). Virtualization.framework's virtio-fs — measured the same way, with a probe binary executed inside a real vzf guest — carries modes in the permission bits and writes no record.
- **The export now reproduces the mode the guest set**, reading the share's record where the permission bits are a placeholder and the bits themselves everywhere else, and applying it with an explicit `chmod` (an `O_CREATE` mode is masked by the exporting process's umask, which would have shaved an observed `0755` to `0700` under a `0077` daemon). **No mode is invented** — the change was which of two real host-side observations to trust, and the guest still does not participate in an export.
- **The sidecar says which observation it used.** An entry whose mode came from the share record is marked `"mode_source": "share-record"`; a plain host stat stays implicit. It is excluded from the tree hash, so the same tree exported from libkrun and from vzf hashes identically. Exported directories are still created `0750` whatever the guest set. Documented in `mgit sandbox export --help`, the `mgit_sandbox_export` MCP tool description, and ADR-011 (with the measurement table).

### Fixed — a restrictive daemon umask no longer strips modes in EITHER direction

- **The inbound twin of the export defect below, found by looking for it.** Worktree staging copied with `O_CREATE`, whose mode argument the kernel masks with the calling process's umask. `mgit-sandboxd` is a long-lived daemon and does not control the umask it inherits, so under `0077` a host `0755` build script was delivered into the guest at `0700` — an executable the agent then cannot run, with nothing anywhere reporting that the mode changed. Both directions now apply the mode with an explicit `chmod`. Nothing had ever asserted a delivered mode, which is why the same defect sat in both halves. (MGIT-81)

### Fixed — firecracker revoke, proven on hardware

- **`mgit sandbox policy` revoke is now hardware-proven on firecracker too**
  (MGIT-78, closing a 0.4.3 known limitation). Both halves pass on real KVM:
  the held, data-carrying flow dies by default (`killed=1`,
  `PROBE-RESULT HOLD = DIED`), survives under `--drain` (`killed=0`,
  `HOLD = SURVIVED after=20.042s`), and in both cases the next flow is refused
  by policy — connect-then-reset here, as this backend's REDIRECT-to-proxy
  design predicts, not libkrun's connect-refused. Recorded in
  `docs/adr/012-live-egress-policy-mutation.md`.
- A latent flake in `TestE2E_PortPublish_GuestServiceReachableOnHost`, exposed
  the first time the battery ran in CI: the host publisher listener accepts
  unconditionally, so a single dial could succeed in the window where the
  guest's `nc -ll -w 1` had timed out, and read nothing. The assertion now
  polls for the guest's bytes, which were always the actual evidence.

### Fixed — `brew install` was broken for every macOS user without libkrun

- **`brew install hyper-swe/tap/mgit` installed nothing at all on a fresh Mac.** The formula declared `depends_on "libkrun/krun/libkrun"`, and Homebrew resolves dependencies *before* it fetches anything and refuses to load a formula from an untrusted third-party tap. The install aborted with exit 1 and no binaries — not even core mgit, which is CGO-free and never links libkrun. A user who wanted only the commit substrate was blocked by a VMM they would never run, contradicting the formula's own caveats ("Core mgit … is ready to use"). **libkrun is no longer a formula dependency**; core mgit installs with one command, no second tap and no `brew trust`. The sandbox is a documented second step and still fails closed: the daemon cannot load without libkrun, and `mgit` surfaces the dynamic loader's error together with the commands that fix it. (MGIT-75)
- **The libkrun install commands we published did not work either.** `brew install libkrun` fails on the untrusted-tap gate — the bare name does not match the formula's full name for Homebrew's explicit-request escape hatch — and the fully-qualified `brew install libkrun/krun/libkrun` fails one step later on `libkrunfw`, libkrun's own dependency from the same tap, which no command-line argument can whitelist. The remedy message, formula caveats, README, install guide and release checklist now all give the sequence that actually runs: `brew tap libkrun/krun`, `brew trust libkrun/krun`, `brew install libkrun`. (MGIT-75)
- **Why this was invisible.** `brew install --dry-run` *passed* on the machine that later hit the failure, because libkrun was already installed there and a satisfied dependency is never loaded — the same species as 0.4.2's MGIT-69/70, where the state that makes the check pass is the state a first-hour user does not have. CI now installs the repo formula on a macOS runner with **no** libkrun (`scripts/brew-install-no-libkrun.sh`), and that check asserts libkrun's absence before drawing any conclusion from its own result, so it fails loudly rather than passing vacuously. (MGIT-75)

## [0.4.2] - 2026-08-09

Three defects of the same shape as 0.4.1's, all found by asking what a guest can actually *do* rather than what the host can demonstrate. Two of them were in the shipped Linux backend; one was in the 0.4.1 fix itself.

### Fixed — firecracker (Linux) guests had no resolver

- **The guest had a route but no `/etc/resolv.conf`.** firecracker's guest gets its address and default route from the SDK's `ip=` kernel boot parameter, but the kernel writes that parameter's nameservers to `/proc/net/pnp`, **not** `/etc/resolv.conf` — and the guest rootfs ships no `/etc` directory at all (verified by reading the image, not inferred). So every `getaddrinfo` caller in the guest — npm, apt, curl, git — had no resolver, the same `EAI_AGAIN` half of MGIT-68 on a different backend. The host now sends a **resolver-only** `guestboot` descriptor and the guest-side machinery added in 0.4.1 applies it. Resolver-only is deliberate: the kernel's `ip=` already addresses the guest and that mechanism works, so mgit does not send a second, redundant link configuration. (MGIT-69)

### Fixed — allowlist mode was unusable from inside a firecracker guest

- **An ordinary program could not connect to anything, allowlisted or not.** Allowlist mode gave the guest no direct route and permitted exactly two host ports: mgit's DNS resolver, and mgit's **length-prefixed CONNECT proxy** — a protocol nothing in any guest speaks. No proxy environment variables were injected and there was no redirect, so `npm install` and `curl` had no way through the policy at all. The enforcement was real; there was no door. The e2e drove the proxy from the **host**, so it demonstrated the policy while never proving a guest could use it.
- **The guest's TCP is now REDIRECTed into a transparent proxy** that authorizes each flow on the **original destination recovered from conntrack** (host-observed via `SO_ORIGINAL_DST`, never guest-asserted — SEC-05) and dials the **pinned** address the authorizer returns (DNS-rebind defense, SEC-04). This gives firecracker the same semantics libkrun's netstack gateway already had, so one policy now means one thing on both backends. The default-deny is unchanged: the redirect changes *how* a flow reaches the authorizer, never *whether* policy decides it. A denied flow is **reset** (`SO_LINGER 0`) rather than closed cleanly, so a policy refusal stays distinguishable from an empty reply. (MGIT-69)

- **Behavioural note — what a denial looks like differs by backend, by construction.** On firecracker the kernel's REDIRECT completes the TCP handshake with the local enforcement point *before* mgit decides, so a guest program sees a successful `connect()` followed by a **reset with no data** (curl reports "Empty reply from server"). On libkrun the userspace gateway resets during the handshake, so the guest sees **`ECONNREFUSED` at connect**. Both are policy refusals and both remain distinguishable from a dead network — which reports "unreachable" — but a script keying on the exact errno will see different values per backend. The substantive guarantee is identical: no bytes from the destination, and nothing dialed toward it.

### Fixed — open mode resolved nothing, on both backends

- **0.4.1 pointed every networked guest's `/etc/resolv.conf` at the gateway, but open mode bound no resolver there** — so every name in open mode failed with "connection refused" against a dead port. The 0.4.1 open-mode test dialled a **raw IP**, which is why it could not see this: a raw IP is not a substitute for a name, they exercise different halves of the stack. Both backends now serve an open-mode resolver on the gateway. It lifts **which names resolve** and nothing else, so open-mode DNS is now *audited*, rate-limited (SEC-07 anti-tunnel) and subject to the unconditional IP denials — which it never was when the guest resolved through NAT on its own. (MGIT-69)

### Fixed — vzf silently ignored the allowlist policy (SEC-04 false containment)

- **`--network allowlist` on the vzf backend granted UNRESTRICTED egress.** vzf attaches a macOS NAT device whenever the mode is not `none`, and there is no host tap, firewall, egress proxy or pinning resolver on that path — `wireEgress` is a no-op off Linux, so nothing else supplied them. The user asked for a filtered network and got an open one, with no error. vzf now **refuses** allowlist mode, naming the backend that can enforce it (the default libkrun) — the same call the container backend already makes for the same reason, because silently approximating a containment policy is worse than declining it. `none` and `open` are unaffected; open is honestly unrestricted by definition. (MGIT-70)

### Known limitations

- **vzf guests are still unaddressed in `open` mode.** Its address comes from macOS vmnet's own DHCP server, and `mgit-guest` (PID 1) has no DHCP client, so 0.4.1's static descriptor does not transfer. vzf is a non-default build (`-tags vzf`; libkrun is the macOS default), and closing this needs either a DHCP client in the guest or a host-side lease query — more than a modest change, so it is filed rather than rushed. (MGIT-70)

### Tests — the scaffolding that hid all of this

- **Two more pieces of test scaffolding were masking the production path**, both the same species as the self-configuring guest that hid MGIT-68. Every firecracker network probe ran `ip addr add` + `ip route replace` **itself** (`netUpPrefix`) before probing, so whether the production boot path addresses the guest at all had never been asserted anywhere. And every DNS assertion passed the server **explicitly** (`nslookup <name> <gateway>`), which bypasses `/etc/resolv.conf` — the path a real caller takes. The new real-VM tests use neither: they assert the guest's own view, after the production boot path, with an **allow assertion for every deny assertion**, and a denial that reports "unreachable" now **fails** as a dead network rather than an enforced one.
- **`TestE2E_PortPublish_GuestCannotReachHostLoopback` was negative-only** and would have passed on a guest with no working network at all. It now proves the guest's network is alive against a control listener on the gateway *before* asserting it cannot reach host loopback (SEC-09).

### Added

- **Host worktree edits now reach a running guest** (MGIT-71). The guest is a
  staged copy, not a live mount — that is what makes the SEC-03 quarantine
  enforceable — so propagation re-stages through the same host-side checks a
  launch uses: a sync can never deliver what a launch would have refused.
  Collision policy, decided rather than defaulted: host-delivered files the
  guest has not touched are updated; a file changed on both sides is a
  **conflict** and the whole sync is refused with the paths named (`--force`
  overwrites and reports every path it destroys); **guest-created files are
  never touched**, so a build cache survives a sync. Reported by the HyperSwe
  lane, whose loop this unblocks.

### Fixed

- **A data race in egress authorization** (MGIT-72). `AllowsName`/`HasName`/
  `AllowsIP` read the compiled rule set without a lock. That was safe only
  while rules were immutable after `Compile`; it is now guarded. Pre-existing
  and latent — surfaced by the race detector while making policy mutable.

### Known limitations

- **Live policy revoke is not user-facing yet.** The enforcement core landed
  (kill established flows by default, `--drain` opt-in) but there is no CLI or
  MCP surface, and it does **not work on libkrun** — the macOS default — where
  the authorizer runs inside the VM child process and the daemon has no route
  to it. Deliberately unexposed rather than shipped as a verb that silently
  does nothing on the default platform. MGIT-74 tracks the control channel;
  MGIT-72 ships when it works everywhere.
- **Host→guest sync is libkrun-only.** Firecracker delivers the worktree as an
  ext4 image built at launch, which the host cannot write into; those sandboxes
  keep launch-snapshot semantics and report the limitation rather than
  pretending. The host decides *what* to apply; the backend decides *how*.

## [0.4.1] - 2026-08-08

### Fixed — P0: guest egress was non-functional on macOS/libkrun in 0.4.0

- **The sandbox guest never had an IP address.** Nothing configured the guest's NIC: `mgit-guest` (which is PID 1, so no distro init, `dhclient` or NetworkManager ever runs) had no address or route setup at all, and the netstack gateway serves no DHCP. Under libkrun there is additionally no kernel command line of mgit's to carry an `ip=` parameter — libkrunfw supplies the cmdline, which is why the boot descriptor already travels in the guest environment. So `eth0` came up **present but unaddressed, with no default route**. One cause, both reported symptoms: `ENETUNREACH` dialing a raw IP in `--network open`, and `EAI_AGAIN` for every name in `--network allowlist` because the gateway's `:53` was equally unreachable. Reported by the HyperSwe lane (hyper-swe/swe#66). **The gateway was never the problem** — it is proven against a real VM; the guest side of the wire was missing. (MGIT-68)
- **The guest is now told its address at boot, over the boot-token channel it already uses.** `mgit-guest` configures `eth0` (address, netmask, up, default route) and writes `/etc/resolv.conf` pointing at the gateway resolver, from a new `mgit.net_*` descriptor the host composes. **Addressing choice, and why:** static configuration from the host, not DHCP. mgit-guest *is* the guest userspace and runs before anything else, so DHCP's usual advantage — working with an init you do not control — does not apply here, while its cost would be a DHCP server written into the security-critical gateway (gvisor's netstack ships none) plus a client in the guest: new code on both ends of a trust boundary to deliver three values the host already knows. Revisit only if mgit ever hands PID 1 to a base image's own init. **Single source of truth:** the values are derived from the gateway's own `guestIP`/`gatewayIP`/`gwPrefixLen` constants and transported, never restated — two copies of an address is how this breaks again.
- **`none` mode is unchanged and still gets no address**, deliberately: its NIC exists only to keep libkrun off its fail-open TSI fallback and is backed by a discard socket, so there is no gateway to point a guest at.

### Fixed — the test gap that let it ship, which matters as much as the fix

- **Every real-VM network test asserted only that a DENIED destination was denied.** An unconfigured NIC produces exactly that result, so `TestE2E_Libkrun_RealVM_NoneMode_NoEgress` and `..._Allowlist_DefaultDeny` passed while proving nothing, and there was **no real-VM assertion anywhere that an allowed flow succeeds**. Four now exist, all passing on real hardware (macOS/HVF, Apple Silicon): an allowlisted destination returns **real bytes**; an allowlisted **name** resolves through the gateway resolver and connects to the **pinned** address (SEC-07); `open` mode reaches a **raw IP**; and the off-list flow is refused with an asserted **reason** — `connection refused` (a policy reset), where `network is unreachable` now **fails** the test as a dead network masquerading as an enforced one. The guest in these tests is the real `mgit-guest` and the probe is exec'd in over vsock, so it configures nothing itself — it can reach anything only if the production boot path worked.
- **The rule going forward: every deny assertion needs a matching allow assertion, or "blocked" is indistinguishable from "broken."** The pre-existing deny tests were kept and strengthened rather than replaced (`none` mode now additionally asserts the guest *had* an address and a route and still got nowhere, so that claim is about none mode rather than about an unconfigured NIC).

### Known limitations (unchanged by this release, recorded here because they are the same shape)

- **firecracker (Linux) writes no `/etc/resolv.conf` either.** Its guest gets an address and route from the kernel's `ip=` autoconfiguration (supplied by the firecracker SDK), so routing works — but nothing writes a resolver file, and its allowlist e2e only ever resolves with the server named explicitly (`nslookup <name> <gateway>`), which does not exercise the default-resolver path a real `getaddrinfo` caller (npm, curl, apt) uses. Not changed here because it could not be live-validated on KVM in this cycle, and an unvalidated change to the working Linux backend does not belong in a P0 hotfix. Filed with the evidence.
- **vzf (macOS, `-tags vzf`, non-default) has the same unconfigured-NIC shape as libkrun did.** It attaches a NAT NIC and nothing configures the guest; its address is vmnet-assigned, so the static descriptor used here does not apply unchanged. Filed.

## [0.4.0] - 2026-08-06

**Platform scope, precisely:** the microVM sandbox links **libkrun by default
on macOS 14+** and **firecracker on Linux** (KVM-only, root not required for
launch) — different VMMs by default, not a symmetric feature. **Windows has
no sandbox in this release**; core mgit (worktrees, commit, squash, rollback)
runs there without containment (WCOW backend planned, MGIT-12). The guest
**base userspace is fetched via OCI** (`mgit sandbox base from <image>`) — mgit
redistributes no kernel or userspace image. The **guest-image bundle publish
remains on HOLD** (MGIT-61.12, owner decision 2026-07-29): the reproducible
build and local install path (`mgit sandbox image install --from <bundle>`)
are done and live-validated, but `gh release upload`-ing the bundle itself is
a deliberate one-way door the libkrun consolidation isn't ready to commit to
yet — see the Release checklist for what stays undone until that gate lifts.

### Security

- **gvisor pin checked against Tailscale's current release for the first time (release-checklist gate, previously never run).** As of 2026-07-31, Tailscale `v1.102.0` pins the exact same `gvisor.dev/gvisor v0.0.0-20260224225140-573d5e7127a8` mgit already carries — **held, no bump needed.** `go build ./...` and `go test ./internal/sandboxd/... -count=1` both pass at this pin (egress package included — the netstack forwarder enforcing default-deny/allowlist, SEC-04). Re-run this comparison every release; see docs/release/RELEASE-CHECKLIST.md.
- **Storage-engine dependencies upgraded to clear 7 disclosed vulnerabilities in called code.** `go-git/v5` 5.17.2 → 5.19.1 (GO-2026-5693 malformed-object panic/resource-exhaustion, GO-2026-5496 SSH transport escaping, GO-2026-5074 improper tree-entry parsing) and `go-billy/v5` 5.8.0 → 5.9.0 (GO-2026-5597 path traversal, GO-2026-5490 symlink-cycle DoS), plus the Go toolchain 1.26.4 → 1.26.5 (GO-2026-5856, `crypto/tls`). `govulncheck ./...` now reports 0 vulnerabilities in called code. Both dependencies stay within their already-approved v5 line (APPROVED-PACKAGES.md floors unchanged; v6 remains unapproved). Re-verified with the full test suite, `-race`, and a live firecracker e2e run on real KVM hardware — no regressions.
- **Fixed a latent defect in tree-flattening surfaced by the go-git upgrade** (`internal/store/git/plumbing.go`): `flattenTree` treated *any* error from go-git's tree walker as ordinary end-of-iteration and silently discarded it, returning an empty result instead of an error. GO-2026-5074's fix made go-git's walker itself start rejecting forged tree entries (a `..` path component, a disguised `.git` name, control characters) that earlier versions passed through for mgit's own escape check to catch downstream — surfacing the bug: a forged-tree checkout now went from "escapes caught downstream" to "the walker errors, and we silently swallowed it," which (since `materializeCommit`'s cleanup pass deletes tracked files absent from the target) could have turned a blocked, escaping checkout into a silent deletion of the whole working tree instead. Only `io.EOF` now ends the walk successfully; every other error aborts the checkout. The path-escape guarantee itself was never broken — go-git's own new validation caught it either way — but the failure mode this fixes (silently-empty target tree) was real and worse than a blocked checkout.

### Changed — GA backend split: libkrun is now the default on macOS (ADR-010)

- **`mgit-sandboxd` on macOS now links `libkrun` by default — no build tag required.** This is the single biggest change since 0.3.1-beta and affects every macOS install: `brew install hyper-swe/tap/mgit` now pulls in `libkrun/krun/libkrun` as a hard dependency (macOS 14+, up from the vzf floor of 13+), and the codesign entitlements plist carries both `com.apple.security.hypervisor` (libkrun) and `com.apple.security.virtualization` (vzf) so one signed binary covers either. `make check-libkrun-net` runs as a release before-hook so a libkrun built without networking support fails the *release*, not every user's Mac (libkrun gates `krun_add_net_*` behind an opt-in build flag the Homebrew tap already sets). The older Virtualization.framework (vzf) backend remains in the tree behind the explicit `-tags vzf` and is **not** deleted — it keeps the `microvm.Hypervisor` seam exercised and covers a CGO-off macOS build, where libkrun cannot link at all. (MGIT-61.13, MGIT-61.14, ADR-010)
- **Linux stays on firecracker.** libkrun is unpackaged on every current Ubuntu release and its real-VM boot did not complete on Linux/KVM as of this decision (see "Known limitations"); flipping Linux now would ship a backend that cannot boot. `mgit-sandboxd` there requires the explicit `-tags libkrun` to link it at all. Every platform wiring (firecracker, vzf, libkrun) now logs which VMM it linked at daemon start, since the choice is a build-time fact with no runtime flag.
- **Building `mgit-sandboxd` from source on macOS now needs libkrun's `pkg-config`.** `make build`/`test`/`lint` derive `PKG_CONFIG_PATH` from Homebrew automatically; a raw `go build ./cmd/mgit-sandboxd/` needs it exported by hand (documented in `docs/INSTALL-SANDBOX.md`). Core `mgit` is unaffected — it stays `CGO_ENABLED=0` and never links libkrun.

### Added — bring your own guest userspace: `mgit sandbox base from <oci-image>`

- **`mgit sandbox base from debian:12` composes this repo's guest base from any public OCI image.** It pulls the image straight from its registry — no Docker, no container runtime, no daemon — extracts the layers into `.mgit/sandbox/base/`, injects `mgit` and `mgit-guest`, then pins the composed tree by content digest and signs it into `images.lock`. Because YOU pull the image, mgit redistributes nothing: no kernel, no userspace, no GPL corresponding-source obligation. Pick the image your task's toolchain needs (`node:22`, `python:3.12`, `golang:1.23`) — the base *is* the environment the agent works in. (MGIT-61.15)
- **`mgit run` and `mgit work --sandbox` boot the registered base automatically.** `--image` is now optional; a digest is the output of composing a base, not something a person should carry between commands. With no base registered they fail **closed**, naming the one command that fixes it — mgit ships no default base, and silently booting an image you never chose would put unreviewed code inside your containment boundary.
- **The release archive now carries linux builds of `mgit` and `mgit-guest`** in a `guest/` directory beside the host binary (never on `PATH`), which is what makes a plain `brew install` enough to compose a base with no Go toolchain and no source checkout. These are mgit's own Apache-2.0 binaries; the kernel and userspace still ship from nobody.
- **Provenance is signed, not just recorded.** The resolved OCI reference — registry, repository, tag, and the digest the registry actually served — is stored on the lock entry *and* covered by its Ed25519 signature, so an entry cannot be edited to claim it came from `debian:12` while booting something else. The tree digest remains what boot verifies, and it is re-verified on every resolve.
- **Refusals and warnings that fire at compose time rather than hours later**: an image built for another architecture is refused naming both (libkrun is hardware virtualization — there is no emulation to cross architectures with); a scratch/distroless image is refused by name (no shell means no agent command can run); a musl base warns, because a glibc-linked tool inside one dies with "no such file or directory" naming its dynamic *loader*.

### Added — the guest base is now part of what a commit's attestation says

- **A commit's host attestation now names the guest base the sandbox booted**, signed alongside the commit and content hashes it already covered. The host previously attested *what* was produced and *where*, but said nothing about the userspace it was produced in — which matters now that a base is whatever the user pulled from a registry. The digest comes from the launch record the host itself wrote, never from anything the guest can influence (SEC-01). (MGIT-61.15)
- **Attestations signed before this field existed still verify.** The signing payload records which layout it was signed over, and the new fields are appended to exactly the original bytes, so an older record hashes to precisely what was signed. Stripping the digest and the version marker together yields the older layout — a different byte string from the one signed — so a downgrade fails the signature rather than passing silently. The mirror attack is refused structurally: a base digest present under a layout that does not sign it is rejected before any signature is checked.
- **The signing payload layouts are specified** in `IDD-FR17-SANDBOX-PROTOCOL.md` §3.2–§3.4, including the rule every future layout must follow (append only, never reorder or remove) so an independent verifier (FR-17.32) can be written against them.
- **Scope, stated plainly:** the attestation is verified in flight during a land and is not itself persisted, so the *durable* answer to "which base produced this commit" is still the join from `task_commits.sandbox_id` to the launch record's `image_digest`. What the signature adds is that the claim cannot be forged or altered while it is being relied on.

### Fixed — first-run failures on a clean macOS install

Found by installing a real release archive into a pristine directory with a scrubbed environment: no Homebrew on `PATH`, no `DYLD_*`, no `PKG_CONFIG_PATH`. (MGIT-61.14, MGIT-61.15)

- **The daemon could not start in a fresh repo.** `.mgit/sandbox` is where its audit index, policy and trust root live, but nothing created it, so any `mgit sandbox …` command before `mgit sandbox image init` died with SQLite's `unable to open database file: out of memory (14)`. The index store now creates the directory it lives in, as every other store in the repo already did.
- **Every activation failure looked the same** — `d.sock not dialable after spawn` — whether the cause was a missing libkrun, a missing directory or a corrupt database. The daemon's output is now captured beside its socket and reported with the error, structured records rendered as plain sentences rather than raw JSON.
- **A Mac without libkrun now gets an actionable message.** That failure is invisible from inside the daemon — the dynamic loader rejects the binary before `main()` runs, so no in-process capability check can fire — and it is the most likely first-run failure there is. The user now sees the loader's error *and* `brew tap libkrun/krun && brew install libkrun`.

### Fixed — five defects that stopped any real base from booting on macOS

Found by composing a base from `debian:12` and running a command in it; none were visible to a green test suite. (MGIT-61.15)

- **Absolute symlinks were treated as tree escapes.** Inside a guest root, `/etc/alternatives/awk -> /usr/bin/mawk` resolves against the *guest's* root, so it names a file in the base — and `debian:12` ships dozens, which made every realistic base unpinnable. Relative targets climbing out with `..` are still refused.
- **Every socket path on a stock Mac exceeded the 103-byte `sun_path` limit by 13 bytes**, so no VM could boot on a default install: macOS hands each process a 48-byte private `TMPDIR` and a 26-character ULID sat on top of it. The per-sandbox state directory now uses the ID's random tail and the socket names are shorter. The backend's own tests all built state dirs with a short temp dir, which is exactly what kept this invisible.
- **Nothing pointed the VM child at `libkrunfw`.** libkrun `dlopen`s it by leaf name and Homebrew's `/opt/homebrew/lib` is not on macOS's default fallback search path, so the child died with "Couldn't find or load libkrunfw.5.dylib" unless a loader path had been exported by hand. The daemon now finds it; an operator's own value still wins.
- **The guest parsed libkrun's quoted echo of its own boot descriptor.** libkrun renders the workload environment onto `/proc/cmdline` in its syntax, and the guest merged both sources with the cmdline winning — so it read `mgit.worktree_src=work"` and died mounting a virtiofs tag that does not exist. The environment channel is now authoritative.
- **The first command after `mgit work --sandbox` always failed** with a bare `read frame: EOF` while the second worked. libkrun's host-side vsock endpoint exists from VM start, so the request was written into a connection nothing was listening on yet. It is retried now — but only while the guest has never once answered and nothing at all came back, the only state in which the command provably never reached a listener.

### Added — libkrun backend (cross-platform microVM, ADR-010)

- **libkrun backend core + a rootless, portable netstack egress gateway.** Replaces the Linux-only, root-requiring `iptables`/TAP engine with a pure-Go userspace TCP/IP stack (gVisor-based) that enforces the same default-deny/allowlist/open contract on both platforms libkrun supports (and, later, Windows) — one egress implementation instead of per-backend ones. The daemon re-execs itself as a child process per VM (`krun_start_enter` takes over the calling process, so libkrun cannot run VMs in-process), which also means the codesign entitlement is inherited by construction on macOS. (MGIT-61.5, MGIT-61.6, MGIT-61.8, MGIT-61.9)
- **SEC-03 quarantine and SEC-09 port publishing delivered on libkrun.** `CreateVM` now builds the quarantined staging tree (worktree + a private `.mgit`, host store excluded, escaping symlinks rejected) instead of refusing launches carrying a private store; host→guest port publishing goes over `krun_add_vsock_port2(listen=true)`, the same shape as firecracker's. (MGIT-61.6)
- **`mgit-guest` (the same PID-1 supervisor `mgit run` uses on every backend) now boots successfully under libkrun** and serves the real exec/land/notify vsock ports — the previous validation only exercised purpose-built minimal test workloads. Two real defects surfaced only by booting the actual guest: libkrun leaves the initial mount namespace shared (firecracker/vzf leave it private), so making it private had to move before the `/proc` `MS_MOVE`, not after; and a host guest-root directory missing `/proc /dev /tmp /mnt` now fails fast, host-side, naming the missing paths, instead of an opaque in-guest mount error. (MGIT-61.13 P2, MGIT-61.15)
- **`open` network mode is audited on libkrun**, not merely unrestricted: every connection still passes through an authorizer that logs it, matching the allowlist/none modes' audit trail even though open permits everything.
- **The mgit CLI ships inside the libkrun guest image**, so an agent whose shell is routed into the sandbox can `commit`/`status`/`log` against the SEC-03 private store from inside the VM (previously only the ext4-image delivery had this).

### Build

- **Reproducible, SOUP-pinned guest-image build** under `scripts/sandbox-image/`: every external input is pinned by content digest (kernel source sha256, toolchain + busybox image `@sha256`, an explicit kernel-config symbol list) and every non-deterministic knob fixed (`SOURCE_DATE_EPOCH`, `KBUILD_BUILD_*`, a fixed rootfs UUID). The arm64 vz (Apple Virtualization.framework) kernel is **bit-for-bit reproducible** — two builds produce an identical `Image`, asserted against a recorded digest. `build-bundle.sh` emits the install bundle (`manifest.json` + kernel + rootfs) that `mgit sandbox image install` consumes; live-validated end-to-end on macOS (build → install → `mgit run` in-guest). Replaces the ad-hoc `/tmp` build used during the vzf validation. (MGIT-30)

### Added

- **`brew install hyper-swe/tap/mgit` now prints a sandbox-activation caveat** — the per-platform prerequisites plus the single `mgit sandbox image install` command — so a brew user is one documented step from a working sandbox (core mgit works without it). Formula change in the tap. (MGIT-61.3)
- **`mgit sandbox image install` works with no arguments**, defaulting to the latest mgit release's published guest-image bundle (GitHub `releases/latest/download`, so image updates need no code change); `--from` still overrides with a local dir or URL. The reproducible build now produces **all three platform bundles** — `scripts/sandbox-image/publish.sh` assembles a combined `manifest.json` + per-platform kernel/rootfs + `checksums.txt` ready to attach to a release. The firecracker (Linux) kernel is vendored from firecracker-ci pinned by sha256 (a reproducible-by-us firecracker kernel is a follow-up); the vz kernel is built reproducibly (MGIT-30). Live-validated end-to-end from the published bundle on **both** backends: Linux/KVM firecracker and macOS vzf (`install` → `mgit run` in-guest). The publish + release cut are owner-triggered. (MGIT-61.2)
- **`mgit sandbox image install --from <dir-or-url>`** activates the sandbox in one step: it fetches a pinned guest-image bundle for the host platform (a `manifest.json` + kernel + rootfs), verifies each artifact's sha256 (fail-closed on mismatch), auto-creates the signing trust root if absent, and registers the digest-pinned, Ed25519-signed image — no manual `image init`/`add` or kernel build. Idempotent. Part of the sandbox-active milestone (MGIT-61); images are digest-pinned + locally signed (local-trust), with published image bundles and a distribution-signing key tracked as follow-ups. Live-validated end-to-end on macOS (Apple Virtualization.framework): install from a bundle → `mgit run` executes in-guest. (MGIT-61.1)

### Known limitations (unreleased, tracked)

- **libkrun's real-VM boot on Linux/KVM is not yet fully validated end to end.** The backend builds, links, and its unit test suite passes on real KVM hardware, and `mgit-guest` itself now boots correctly there (see above) — but the from-scratch build path was only exercised outside CI, on a manually-provisioned runner, and is not a release gate. Firecracker remains the Linux GA default until this closes. (MGIT-61.13 P4)
- **No current Ubuntu release packages libkrun or libkrunfw** — a from-source Linux build (~4–5 minutes on modest hardware) is required regardless of the boot-validation status above. Debian testing/sid packages the kernel-bundling half (`libkrunfw-dev`); `libkrun` itself is unpackaged on every Debian/Ubuntu release including testing.

## [0.3.1-beta] - 2026-07-07

### Added

- **`mgit serve --project <path>`** targets a specific repo instead of relying on the current working directory — needed by the Claude desktop app, which launches the MCP server from an arbitrary cwd. Claude Code / Cursor still work unchanged (cwd is the default). The README MCP section shows the desktop config. (MGIT-60)

### Fixed

- **Three firecracker e2e workflow tests no longer skip the guest on a stale digest.** They pinned a hardcoded placeholder image digest that no longer matched what `images.Register` computes for the real rootfs, so the launch's `ImageRef` failed verification (~0.2s, before any VM boot) — silently not exercising the guest. They now launch with the actually-registered ref and assert against its real digest. Test-only; the product was unaffected. (MGIT-59)

- **`mgit run` now works end-to-end into the sandbox (first live microVM passes).** Two bugs on the exec path, both invisible to the in-process test harness and caught only by driving a real microVM: (1) the first exec after a lazy launch dialed the guest vsock ~0.4s in, before the guest (~1s to boot) had bound its listener, so the handshake reset — the exec now waits for guest vsock readiness with a bounded retry, at the backend-agnostic seam so both firecracker and Apple Virtualization.framework benefit; (2) the guest resolved a bare command (`echo`, and by extension `npm`) against PID 1's empty `PATH` instead of the child env's, so `mgit run -- <bare cmd>` failed "executable file not found" — the guest supervisor now resolves the program against the guest `PATH`. Verified live on **both** backends (`sandbox_posture.sh` prints PASS (live)): Linux/KVM firecracker and macOS Apple Virtualization.framework — `mgit run -- echo ok` executes in-guest and the land round-trip succeeds. (MGIT-58)
- **`mgit run` inside a worktree now finds its sandbox.** The documented `mgit work ./wt --sandbox` then `cd wt && mgit run` flow failed two ways, both found by the first live sandbox pass: from inside a linked worktree the daemon was keyed on the worktree (not the shared parent), spawning a second daemon against a nonexistent host root that died; and the sandbox's worktree path was recorded verbatim (relative) while `mgit run` matched an absolute, symlink-resolved cwd, so they never matched. Both sides now resolve the owning repo and canonicalize the path. (MGIT-57)

## [0.3.0-beta] - 2026-07-04

### Documentation

- **README and agent docs now match shipped reality.** Added an "Enable the sandbox" section (install `mgit-sandboxd` per channel, guest image, platform prerequisites) and stated in Quick start that `--sandbox` requires it while everything else works without it, with the no-sandbox integration path (`squash --to-git | git apply`). Corrected two wrong commands the docs advertised: the MCP server is `mgit serve --mcp-only` (there is no `mgit mcp`), and the REST API is `127.0.0.1`-only covering a subset of operations (dropped the unimplemented `mgit token generate` bearer-auth claim). The agent working-discipline skill gains pitfalls for the daemon-less posture, sandbox-only landing, the serve lock, and the now-working MCP worktree tools. (MGIT-49)

### Added

- **Install-channel + posture e2e in CI (release gates).** New jobs exercise what a real user gets — an installed binary, no repo checkout — across both postures: the core loop over an installed `mgit` (`squash --to-git | git apply` round-trip included), the daemon-less honest degraded mode, the full MCP tool surface driven through a real stdio client, and a virtualization-gated sandbox pass. A regression like "mgit-sandboxd missing from the archives" or "an MCP tool returns placeholder" now fails CI before users see it. Run locally with `make e2e`. (MGIT-48)
- **The flagship claims now have e2e proof (release gates).** New always-on e2e legs: the course-correction loop end to end (micro-commits with a wrong step → append-only rollback → fork → salvage from a checkpoint → squash, with the abandoned attempt asserted to survive in log + audit); the REST surface driven route-by-route against a real `mgit serve` process; serve/CLI **lock coexistence proven as two real processes** (CLI commit/status/worktree complete promptly while serve runs); and the MCP e2e now **calls every registered tool** through the real stdio server (previously 11 of 15 were only registration-checked). A feature→e2e coverage matrix lives at [docs/E2E-MATRIX.md](docs/E2E-MATRIX.md) so gaps stay visible (Windows and brew-in-CI are listed as uncovered rather than papered over). `core_loop.sh` now actually asserts `mgit status`, which its header claimed. (MGIT-53)
- **`mgit restore --all --commit <hash>`: whole-tree checkpoint recovery.** One command returns the entire working tree to a prior checkpoint's state, comparing against the DISK (not the committed tree), so it recovers a trashed-but-uncommitted tree — the scenario checkpoint recovery exists for. It refuses when uncommitted changes would be overwritten unless `--force` is passed (recovery is that overwrite, made explicit). Nothing is committed; the restored paths are STAGED so the next commit lands them as the task's step and the auto-resync never absorbs them anonymously. (MGIT-55)

### Fixed (documentation honesty)

- **The README's course-correction steps now match verified CLI behavior.** e2e-proving the loop surfaced that `mgit checkout <hash>` does not exist (checkout is branch-only), `rollback` reverts the owning task as an append-only record (and does not restore content), and `cherry-pick` records provenance rather than materializing bytes — salvage is `mgit restore <file> --commit <hash>`. The diagram, steps, and command tables now describe the loop that actually works; the underlying product gaps are tracked (MGIT-54 content-restoring rollback/cherry-pick, MGIT-55 whole-tree checkpoint recovery, MGIT-56 status-time auto-sync emptying a task's first-commit diff). (MGIT-53)

### Fixed

- **`mgit rollback` now actually restores content.** Previously the revert commit's tree was byte-identical to the pre-revert tree: the service computed inverse diffs but the store built every tree from staging only and dropped them, so rollback recorded intent without recovering anything. Rollback now reverts the task's NET change into the new commit's tree **and** the working directory. Conflict-safe on three axes (hardened by an adversarial pre-land review): a path changed by a LATER commit refuses; a path changed by a commit INTERLEAVED between the task's own commits refuses (the net would silently destroy that work); uncommitted local edits refuse. File modes are state too: a chmod-only task is revertible, and a regular-to-symlink type change is restored as the original type, not a garbage symlink. Append-only as before; a net-empty rollback is a clean error and mints nothing. (MGIT-54)
- **`mgit cherry-pick` now materializes the picked change.** It applies the source commit's real tree diff onto the current branch (working directory included), with the same conflict safety, instead of writing a provenance-only record whose content never arrived. `--onto` now switches via a real checkout (materialized target tree) rather than a bare ref move. (MGIT-54)
- **Content-applying commits are crash-journaled.** An interruption between the ref advance and the disk/index writes used to leave the tip and working tree divergent, which the auto-resync would absorb, silently undoing the revert. Rollback and cherry-pick now journal the application; the next mgit command completes it (materialize + index) under the lock before anything can observe the divergence. (MGIT-54)
- **Checking `mgit status` before a task's first commit no longer empties the task's diff.** The ADR-008 status-time resync used to absorb staged work-in-progress into the `[mgit-sync]` base, so the task's first commit had no delta (silently losing the review surface and squash content). Staged paths are now treated as pending task work and excluded from base absorption; the base advances on project state only. (MGIT-56)

### Changed

- **Dead REST auth code removed; trust model made explicit.** The unwired `TokenStore`/Bearer middleware (a security control that was present but never enforced) is deleted, and the REST API's real model is now stated everywhere: it always binds `127.0.0.1` (hardcoded; the former `api.bind_address` config key is gone) and is unauthenticated by design — its callers are same-user local processes, the same trust as running the CLI. NFR-5.11 amended with the decision; the token-lifecycle spec is retained there for reinstatement if remote access is ever offered. The `api.http_port` config key now actually works: `mgit serve` uses it when `--port` is not passed. (MGIT-51)
- **REST formally scoped as a minimal same-host integration surface** (health, commits, task commits, branches, squash artifact, rollback, verify) — the parity matrix's REST gaps are now a recorded decision with rationale, not drift. Expansion requires a named consumer plus the NFR-5.11 auth lifecycle. See [docs/MCP-PARITY.md](docs/MCP-PARITY.md). (MGIT-52)

- **`mgit serve` shuts down when its MCP stdio client disconnects** (stdin EOF) instead of blocking until a signal — a stdio server's client connection is its lifecycle. (MGIT-48)
- **`mgit work` on a machine without the sandbox no longer misleads the agent.** Previously it installed PATH shims that routed every command through fail-closed `mgit run` (so the agent "couldn't even echo") and wrote a CLAUDE.md claiming "your shell already routes through `mgit run`" — even with no sandbox daemon present. Now the wiring is containment-aware: with no `--sandbox` (honest-open) no routing shims or hook are installed and CLAUDE.md states plainly that commands run on the host and how to enable containment; with `--sandbox` (containment requested) the fail-closed routing stays, and the security invariant holds — mgit never silently bypasses a requested sandbox. `mgit work` also prints a single parseable `Containment: …` status line. (MGIT-47)
- **A long-running `mgit serve` no longer starves the CLI.** The server used to hold the exclusive repo lock for its entire lifetime, so with `mgit serve` running (e.g. an agent's MCP server), every CLI command on the same repo failed after a 30-second wait (`another mgit process is running`). The server now acquires the lock **per operation** — the same scope a CLI command holds it — so a driving agent over MCP/REST and a human on the CLI can work the same repo concurrently. A contended-lock error now also names the holding command, not just its PID. See [ADR-009](docs/adr/009-per-operation-locking.md). (MGIT-46)
- **The MCP surface is now GA-quality across the board.** The last stubbed tools — `mgit_status`, `mgit_diff`, `mgit_audit`, `mgit_config` — return real data through the same service layer as the CLI instead of canned placeholders (`"no changes"`, `"working tree clean"`, …). Every tool now validates its arguments as hostile input before touching a service (task ids against the MGIT-41 grammar; worktree paths against traversal / control chars / NUL / oversize; free text against NUL / oversize) and returns structured tool errors. The generated MCP reference (`mgit docs generate`) is derived from the live registered tool set, so it cannot drift; a capability parity matrix (CLI × MCP × REST, with documented gaps) is at [docs/MCP-PARITY.md](docs/MCP-PARITY.md). (MGIT-50)
- **The MCP worktree tools now work.** `mgit_worktree_add`, `mgit_worktree_list`, and `mgit_worktree_remove` previously returned a fake-success placeholder (`"not yet available (Wave 11)"`); a driving agent that relied on them got nothing. They now delegate to the same `WorktreeService` the CLI uses — `mgit_worktree_add` materializes a real task-bound worktree with the ADR-008 pinned fork-base, and the tools return structured JSON / errors. (MGIT-45)
- **The sandbox daemon `mgit-sandboxd` is now shipped by every host channel.** Previously the release built only `mgit`, so Homebrew / `go install` / release-archive users never received the daemon and the microVM containment pillar was uninstallable — an external trial concluded mgit was unusable as a working substrate. Release archives (Linux any arch, macOS arm64) now contain **both** binaries side by side; the macOS daemon is built with CGO and code-signed with the `com.apple.security.virtualization` entitlement on an Apple Silicon runner; `go install github.com/hyper-swe/mgit/cmd/mgit-sandboxd@latest` is documented (with the macOS signing caveat). `mgit-guest` continues to ship inside the guest image, not on host `PATH`. See [docs/INSTALL-SANDBOX.md](docs/INSTALL-SANDBOX.md). (MGIT-44)

## [0.2.1-beta] - 2026-06-29

### Fixed

- **`mgit branch --delete` left a stale branch row in the index**, so `mgit worktree add` for the same task later failed with `branch already exists` and no clean recovery (gc/prune didn't help). Delete now clears **both** the go-git ref and the SQLite index in one operation — and clears a stale row even when the ref is already gone, so an already-stranded task recovers. Branch creation also **self-heals** a stale row and is now atomic across both stores (a failed index write no longer leaves a partial ref behind). (MGIT-42)

### Documentation

- Landing-page README reworked to lead with the benefit in plain language (run agents safely; keep a clean, reviewable history), with an independent-trial testimonial and a two-minute try-it CTA — the deep technical sections remain below.
- The agent skill gains a **"Common pitfalls (and the fix)"** section so an agent working through mgit avoids the known friction (worktrees aren't git repos, build artifacts need `.mgit/seed-include`, `--task-id` flag, etc.) up front.

## [0.2.0-beta] - 2026-06-26

First beta to ship the full agent-substrate **and** containment product. `main`
had run far ahead of `v0.1.0-beta` (the sandbox, `mgit work`, worktree
materialization, and git coexistence were all unreleased); this release makes
that work installable.

### Added

- **Per-task microVM containment (FR-17)** — untrusted installs/builds/tests run in a disposable, hardware-isolated microVM, not on the host. Backends: firecracker (Linux/KVM, live-validated) and Apple Virtualization.framework (macOS arm64, live-validated). Default-deny per-task egress allowlist enforced host-side; verified land (dual-hash + task binding + host-anchored attestation) through an airlock; per-sandbox quarantined `.mgit` with the host store provably unreachable from the guest (SEC-03).
- **`mgit work`** — one command to start an agent on a task: provisions a task-bound worktree and wires the agent's shell to route through `mgit run` into the sandbox (degrades gracefully when no backend is present).
- **`mgit run` / `mgit sandbox …`** — route execution into the task sandbox; launch/list/exec/land/grant lifecycle.
- **Runs over an existing git repo (MGIT-14)** — self-contained `.mgit` store via go-git plumbing; the project's `.git` is provably never mutated.
- **Git-authoritative auto-housekeeping (ADR-008, MGIT-35)** — mgit keeps its `.mgit` base coherent with your current local working state automatically; **no manual `mgit sync`**. New worktrees carry your unpushed local foundation; each task pins its fork-base so a later resync never corrupts its diff; defensive read-only `.git` access.
- **Worktree materialization** — a worktree is a full working copy seeded from `.mgit` (gitignore-aware); `.mgit/seed-include` carries build-required gitignored artifacts (e.g. an embedded `web/dist`). (MGIT-38)
- **REST + MCP sandbox surface**, agent-integration adapters, and `mgit docs generate` agent artifacts.

### Changed / Fixed

- **`mgit version`** now reports real build metadata (version/commit/date via ldflags, with a `debug.ReadBuildInfo` fallback for `go install`/`go build`) instead of `dev (commit: none, built: unknown)`. (MGIT-40)
- **Task-id flag standardized** — `--task-id` is canonical on every command; `--task` is accepted as a hidden back-compat alias. (MGIT-37)
- **`mgit squash --to-git` round-trips** — emits a real `git diff` patch that `git apply` / `git am` accepts byte-for-byte. (MGIT-33)
- **`mgit add` / `mgit status` honor `.gitignore`** — no more staging build junk. (MGIT-32)
- **Task-id grammar broadened** — accepts ids like `MTIX-30-probe` and `MTIX-30.6`, with a clear, actionable error on unsafe input. (MGIT-41)
- Positioning corrected away from "safety-critical / DO-178C": mgit is the checkpointed, sandboxed working substrate for LLM coding agents.

### Known limitations

- The microVM sandbox is **Linux + macOS** in this beta; Windows runs core mgit (worktrees / commit / squash) without the sandbox (WCOW backend planned).
- macOS containment runs a **Linux guest** via Virtualization.framework; a mac-native profile is planned.
- The backtrack / fork / cherry-pick course-correction loop is cheap and instructed, but not yet validated as something agents reach for autonomously — reviewer-driven today.

## [0.1.0-beta]

### Added

- **CLI**: 22 commands covering the full mgit workflow — init, commit, log, status, show, branch, config, squash, rollback, verify, audit, add, export, cherry-pick, restore, checkout, merge, gc, import, worktree, docs generate
- **REST API**: 10 endpoints on localhost:6860 with Bearer token authentication, ULID request IDs, and JSON error responses
- **MCP Server**: 15 tools for LLM agent integration via stdio transport (commit, rollback, squash, status, log, show, branch, verify, diff, export, audit, config, worktree add/list/remove)
- **mtix Integration**: HTTP client for mtix REST API, bidirectional task-commit synchronization, auto-squash on task completion
- **Agent Worktrees**: Linked worktree support for multi-agent parallel development with task binding isolation (FR-16)
- **Documentation Generator**: `mgit docs generate` produces 9 agent-facing documentation files (CLI reference, MCP tools, SKILL.md, workflow guides, troubleshooting)
- **Token Authentication**: `mgit token generate/rotate/revoke/list` with SHA-256 hash storage and Bearer middleware
- **Integrity Verification**: Dual-hash model (SHA-1 + SHA-256), commit chain verification, index consistency checks
- **Append-Only Audit**: Immutable task_commits table, structured audit log, rollbacks via revert commits
- **Build Pipeline**: GoReleaser cross-compilation (6 platforms), GitHub Actions CI/CD, cosign signing, Homebrew tap integration

### Performance

- Commit creation: 0.39ms (target <5ms)
- Log query (100 commits): 1.1ms (target <50ms)
- Squash (10 commits): 0.63ms (target <500ms)
- Verify (50 commits): 0.61ms (target <1s)

### Technical

- Pure Go, zero CGO dependencies
- go-git v5 for embedded git engine (no external git binary)
- modernc.org/sqlite for pure Go SQLite (WAL mode, SYNCHRONOUS=FULL)
- 530+ tests, zero race conditions, zero lint warnings
- 85%+ code coverage across all packages
