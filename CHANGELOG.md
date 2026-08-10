# Changelog

All notable changes to mgit are documented in this file.

The format is based on [Keep a Changelog](https://keepachangelog.com/en/1.1.0/),
and this project adheres to [Semantic Versioning](https://semver.org/spec/v2.0.0.html).

## [Unreleased]

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
