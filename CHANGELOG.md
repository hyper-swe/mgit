# Changelog

All notable changes to mgit are documented in this file.

The format is based on [Keep a Changelog](https://keepachangelog.com/en/1.1.0/),
and this project adheres to [Semantic Versioning](https://semver.org/spec/v2.0.0.html).

## [Unreleased]

### Security

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
- **Attestations signed before this field existed still verify.** The signing payload records which layout it was signed over, and the new fields are appended to exactly the original bytes, so an older record hashes to precisely what was signed. Stripping the digest and the version marker together yields the older layout — a different byte string from the one signed — so a downgrade fails the signature rather than passing silently.

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
