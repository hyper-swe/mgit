# mgit release checklist

Releases are **owner-triggered**. This is the gate list: what CI proves
automatically, and what a human must verify live before publishing.

**Changed 2026-08-10 (MGIT-78): the Linux live pass is now automated.** This
document used to say the sandbox "cannot be exercised on a GitHub-hosted
runner", and `e2e.yml` encoded that belief — its sandbox job did not even build
`mgit-sandboxd`, so it only ever ran `sandbox_posture.sh`'s SKIP branch. The
assumption was stale and had never been re-measured; the cost was that the
Linux gate depended on one physical dual-boot box, and blocked releases whenever
it was unavailable. It has now been measured: hosted `ubuntu-latest`,
`ubuntu-24.04` and `ubuntu-22.04` all expose `/dev/kvm` (`KVM_GET_API_VERSION`
= 12, `KVM_CREATE_VM` succeeds), and real firecracker microVMs boot there. The
Linux (KVM/firecracker) pass is therefore a **CI gate on every push, PR and
release**, not a manual step. **macOS/libkrun remains manual**, because there is
no entitled Apple Silicon runner.

## Automated gates (must be green)

Run on the release tag via `.github/workflows/release.yml`:

- **preflight** — `go test ./... -race`, `golangci-lint`, `govulncheck`,
  coverage, anti-stub grep.
- **e2e** (`.github/workflows/e2e.yml`, reused as a release gate — MGIT-48):
  - **posture** — core loop (init, work, commit, `squash --to-git | git apply`
    round-trip, worktree add/list/remove, verify, audit) + daemon-less posture
    (honest wiring, `mgit run` fails-closed with an install pointer) + the MCP
    surface driven through a real stdio client (all documented tools registered
    and working, no placeholders).
  - **install-channels** — `go install` of **both** binaries and a
    release-archive extraction, each running the core loop.
  - **sandbox-live-linux** — the **live Linux containment gate**: real
    firecracker microVMs on real `/dev/kvm`. It builds `mgit` *and*
    `mgit-sandboxd` (no `-tags libkrun`, so the daemon links firecracker),
    installs the sha256-pinned firecracker VMM
    (`scripts/sandbox-image/fetch-firecracker.sh`), builds the pinned guest
    kernel + reproducible rootfs, then runs `scripts/e2e/sandbox_posture.sh`
    and the whole `internal/sandboxd/backend/firecracker` `TestE2E_*` battery
    twice — unprivileged, then under `sudo -E` for the tap/iptables
    (CAP_NET_ADMIN) half. See "Reading the Linux live gate" below for how to
    tell a real pass from a skip. Refs: MGIT-78

A regression like "mgit-sandboxd missing from the archives" or "an MCP tool
returns placeholder text" fails these gates before a user ever sees it.

## Reading the Linux live gate (a skip is a failure, not a pass)

The failure mode this repo keeps rediscovering is a gate that *skips* and reads
as green — `sandbox_posture.sh` exits 0 on a missing prerequisite by design, and
the firecracker `TestE2E_*` gates (`requireGuestImage`, `requireNetRoot`) call
`t.Skip`, which Go reports as a passing package. `sandbox-live-linux` therefore
asserts positively at four points, and any one of them turns the job red:

1. **`/dev/kvm` must be usable.** The runner user is not in the `kvm` group, so
   the job installs the `static_node=kvm` udev rule and then *asserts* the node
   is readable and writable. If the hosted runner class ever loses nested virt,
   this fails loudly instead of quietly returning to an unrun manual step.
2. **The posture script must print `SANDBOX POSTURE E2E: PASS (live)`.** The
   job greps for that exact line; a SKIP fails the job.
3. **The root-gated battery must skip nothing.** With KVM + root + a guest
   image every gate in the package is satisfied, so a single `--- SKIP` means a
   prerequisite silently vanished; the job greps for `--- SKIP` and fails on a
   hit.
4. **The MGIT-78 live-policy pair is named explicitly** — the job requires
   `--- PASS:` lines for `TestE2E_Firecracker_Revoke_KillsEstablishedFlow` and
   `TestE2E_Firecracker_Revoke_DrainKeepsEstablishedFlow`, then echoes their
   `killed=`/`hold probe`/`post-revoke flow refused` evidence into the log.

**Reproducing it locally or on any KVM host** (the same three lines the job
runs — note `MGIT_GUEST_CMDLINE`, without which the image registers with no
`root=`/`init=` and the only symptom is `guest vsock not ready within 15s`):

```
. scripts/sandbox-image/pins.env
sudo scripts/sandbox-image/fetch-firecracker.sh amd64 /usr/local/bin/firecracker
scripts/sandbox-image/fetch-kernel-fc.sh amd64 /tmp/guest/vmlinux
scripts/sandbox-image/build-rootfs.sh   amd64 /tmp/guest/rootfs.ext4
MGIT_GUEST_KERNEL=/tmp/guest/vmlinux MGIT_GUEST_ROOTFS=/tmp/guest/rootfs.ext4 \
  MGIT_GUEST_CMDLINE="$FC_CMDLINE" bash scripts/e2e/sandbox_posture.sh <bindir>
```

`workflow_dispatch` is enabled on `e2e.yml`, so the whole gate can also be
re-run on demand against any branch.

## Mandatory manual live pass — macOS only

The per-task microVM sandbox is the headline differentiator. Linux is covered by
`sandbox-live-linux` above; **macOS still needs a human**, because there is no
entitled Apple Silicon runner. Since the GA backend split (ADR-010,
MGIT-61.13/.14) the two platforms validate **different VMMs by default**, so a
green Linux gate is not evidence about macOS:

- [x] **Linux (KVM) — firecracker, the Linux GA default: AUTOMATED, no longer a
      manual step.** Confirm the `sandbox-live-linux` job is green on the
      release commit. It covers everything this box used to ask a human to run
      by hand — posture (launch → `mgit run` in the guest → verified `land`)
      plus exec/land round-trip, hostile-guest (SEC-03 ×3), notify auto-land,
      overlay-root writability, provenance/claim-to-land/remove-discard, and
      the root-gated allowlist/open network modes, guest-resolver egress, port
      publishing and live policy revoke. A separate hand-run on a physical KVM
      box is no longer required, and its unavailability must never again block
      a release.
- [ ] **macOS (arm64) — libkrun, the macOS GA default.** libkrun needs
      NEITHER a kernel NOR a rootfs (libkrunfw supplies the kernel; the guest
      base is composed from an OCI image) — do **not** set `MGIT_GUEST_IMAGE`
      or `MGIT_GUEST_KERNEL`/`MGIT_GUEST_ROOTFS` here — they are the Linux
      form, and the script now ignores them on Darwin in favor of composing
      from OCI. On an
      Apple Silicon host (macOS 14+), with the entitlement-signed
      `mgit-sandboxd` and the `guest/mgit` + `guest/mgit-guest` pair from the
      release archive (no tag needed; extract the real downloaded archive,
      not a local build — see the downloaded-archive smoke step below, which
      this reuses):
      ```
      bash scripts/e2e/sandbox_posture.sh <bindir>
      ```
      composes a base from `debian:12` by default (override with
      `MGIT_GUEST_OCI_REF=<image>`) and must print `SANDBOX POSTURE E2E: PASS
      (live)`. **Before 2026-08-05 this script unconditionally required the
      Linux kernel/rootfs env vars, so on macOS it SKIPPED even on a fully
      working, entitlement-signed host — the mandatory macOS live pass never
      actually exercised the shipped libkrun/OCI path.** Confirmed by running
      it exactly as this checklist used to document, on real Apple Silicon
      hardware, immediately before the fix. A SKIP here means the gate did
      **not** run, not that it is optional — do not proceed to publish on a
      skip. Also confirm `make test-libkrun`'s full suite passes on this
      host, and that libkrun's own real-VM e2e (`make e2e-libkrun`) is green.
- [ ] **Linux libkrun (`-tags libkrun`) is NOT part of the release gate.**
      It is not the Linux default and its real-VM boot is not yet fully
      validated end to end on KVM (MGIT-61.13 P4, "Known limitations" in the
      CHANGELOG). Do not treat a green firecracker pass as implying libkrun
      also works on Linux — they are different code paths. `ci.yml`'s
      `libkrun-linux` job (main pushes/PRs + `workflow_dispatch`, not
      `release.yml`) build+vets this tagged path on every change so a
      compile/link regression is caught early — that is a CI check, not a
      release gate, and it never boots a real VM. (Its comment that hosted
      runners have no `/dev/kvm` is now false — see MGIT-78 — but booting
      libkrun there is still unproven, so nothing about its status changes.)
      A green `libkrun-linux` CI job says nothing about whether Linux libkrun
      actually runs; only the macOS live pass above does that for its own
      platform.

> Never refer to the Linux KVM host by its LAN IP in the repo, CI logs, or the
> release notes — call it "the Linux runner". (Since MGIT-78 the Linux live
> gate runs on hosted GitHub runners, so this should rarely come up.)

## Publish steps (owner)

1. Ensure `main` is green and the CHANGELOG `[Unreleased]` section is ready.
   Carry the macOS quarantine remedy (MGIT-64, docs/INSTALL-SANDBOX.md) into
   `.goreleaser.yaml`'s `release.header` (the actual release-notes text) if
   it isn't there yet — that file is out of scope for this checklist edit,
   but the notes it produces are user-facing and easy to forget.
2. Tag and push: `git tag vX.Y.Z && git push origin vX.Y.Z`. This triggers
   `release.yml` (preflight → e2e gate → macOS release build+sign → GoReleaser).
3. **Homebrew tap: version + checksums are automatic; the formula body is
   NOT.** `release.yml`'s `homebrew` job dispatches `{tag, project: "mgit"}`
   to `hyper-swe/homebrew-tap`; its own `update-formula.yml` (verified by
   reading it directly, not inferred) downloads that release's
   `checksums.txt` and rewrites ONLY `version` and the four `sha256` values
   in `Formula/mgit.rb` there. It never touches `install`, `caveats`, or
   `depends_on`. So: a routine release needs **no action here**. If
   `brew/mgit.rb` in this repo changed since the last release (install
   logic, caveats, dependencies), manually copy its body into
   `Formula/mgit.rb` in `hyper-swe/homebrew-tap` and commit it there — that
   repo cannot be pushed to from this one, and nothing else does this sync.
   **Must not touch the `mtix` formula in that repo.**
   - [x] **RESOLVED 2026-08-10, re-verified for v0.4.3.** The two gaps below
         were recorded on 2026-08-05 and have since been closed by the
         MGIT-75 tap sync. Re-check them the same way rather than trusting
         this note: fetch the live formula and diff its body against
         `brew/mgit.rb`, ignoring the generated `version`/`sha256` lines,
         which the tap's own `update-formula.yml` rewrites per release:
         ```
         gh api repos/hyper-swe/homebrew-tap/contents/Formula/mgit.rb \
           --jq '.content' | base64 -d > /tmp/tap-mgit.rb
         diff <(grep -v 'sha256\|version "' /tmp/tap-mgit.rb) \
              <(grep -v 'sha256\|version "' brew/mgit.rb)
         ```
         An empty diff means no manual sync is needed for this release. It
         was empty on 2026-08-10.
         - The formerly-missing `libexec.install "guest"` (MGIT-65) is
           present in the live formula, so a `brew install`-ed mgit CAN
           compose a guest base.
         - The caveats now say macOS 14 and `mgit sandbox base from <ref>`
           (not the Linux-only `mgit sandbox image install`). MGIT-44,
           MGIT-64, MGIT-65.
4. Complete the two live sandbox passes above and note them on the release.
5. **Publish the guest-image bundle** — ⛔ **ON HOLD, DO NOT RUN (MGIT-61.12).**
   The owner decided 2026-07-29 to complete the libkrun path before publishing.
   Publishing is a one-way door: it makes mgit a public distributor of a kernel
   the libkrun consolidation intends to retire, and gives HyperSwe a digest to
   pin that would later need migrating off. It also has an UNMET GPL
   corresponding-source obligation for the re-hosted Linux kernel and busybox.
   Teams that need a sandbox today use a LOCAL install instead — same runtime:
   `mgit sandbox image install --from <local bundle dir>`.
   Resume only when MGIT-61.12's gate is met; the artifact may not be this
   bundle format at all (libkrunfw supplies the kernel, virtiofs supplies the
   root). Steps kept for when the hold lifts:
   ```
   scripts/sandbox-image/publish.sh out/publish        # builds all platform bundles + checksums
   gh release upload <tag> out/publish/*               # attach manifest.json + kernels + rootfs + checksums.txt
   ```
   The install default resolves to the latest release's assets. Then verify on a
   clean host: `mgit sandbox image install` → `mgit run --sandbox -- echo ok`.
   (The vz kernel build needs docker; run `publish.sh` on a machine with it.)
6. Post-publish smoke: `brew install hyper-swe/tap/mgit` on a clean machine and
   confirm `command -v mgit && command -v mgit-sandboxd`.
7. **Downloaded-archive smoke (macOS Gatekeeper quarantine, MGIT-64).** A
   locally built or `scp`'d artifact does **not** reproduce this — a build
   directory and an `scp` transfer never carry the `com.apple.quarantine`
   extended attribute, so testing either one is "verified" wrongly, which is
   exactly how this shipped broken once already. Only a real download
   (browser, `gh release download`, AirDrop) or an explicitly set quarantine
   attribute reproduces it. On a Mac that did **not** build this release:
   ```
   gh release download <tag> -p 'mgit_*_darwin_arm64.tar.gz' -D /tmp/mgit-smoke
   cd /tmp/mgit-smoke && tar -xzf mgit_*_darwin_arm64.tar.gz
   xattr -l mgit mgit-sandboxd        # must show com.apple.quarantine
   ./mgit --version && ./mgit-sandboxd --version
   ```
   Both must run without the `xattr -d com.apple.quarantine mgit
   mgit-sandboxd` remedy (docs/INSTALL-SANDBOX.md) — if either is killed, the
   archive shipped broken again. While a machine without a source checkout is
   already set up for this, also run the full sandbox first-run funnel from
   the extracted archive (`mgit sandbox base from <image>` → `mgit work
   --sandbox` → `mgit run`) — this is the same "installed archive, no Go
   toolchain" precondition MGIT-65 requires, and it is cheap to cover both in
   one pass rather than two.
8. **libkrun networking capability check** (clean machine, sandbox builds only).
   libkrun gates its net-device API behind an opt-in build flag, and mgit
   requires an explicit NIC in every mode — without one libkrun falls back to
   TSI and the guest gets full host egress, so a libkrun built without
   networking cannot host a sandbox at all. The `libkrun/krun` tap passes
   `NET=1` explicitly today, but that is a THIRD-PARTY build flag: if the
   formula ever drops it, every Mac install breaks. Verify per release:
   ```
   brew tap libkrun/krun
   brew trust libkrun/krun   # required: Homebrew refuses to load an untrusted tap
   brew install libkrun
   nm -gU "$(brew --prefix libkrun)/lib/libkrun.dylib" | grep krun_add_net_unixgram
   ```
   A match is required. `make check-libkrun-net` performs the same check at
   build time, and mgit-sandboxd re-verifies it at startup and refuses closed.
   Refs: MGIT-61.14

## Dependency step — re-pin gvisor to the version Tailscale ships (every release)

The sandbox's egress enforcement embeds gVisor's userspace netstack, which sits
directly on the containment boundary — a stale netstack means missing security
fixes in the code that decides what a hostile guest may reach. gvisor publishes
**no semver versions**, so the pin is a deliberate, maintained choice.

**Until mgit forks and maintains its own netstack, track Tailscale's pin.**
Tailscale is the largest production consumer of gVisor netstack, and vets the
version it ships. Before each release:

```bash
# 1. What does the current Tailscale release pin?
TS=$(curl -s https://proxy.golang.org/tailscale.com/@latest | \
     python3 -c 'import sys,json;print(json.load(sys.stdin)["Version"])')
curl -s "https://proxy.golang.org/tailscale.com/@v/$TS.mod" | grep gvisor.dev

# 2. Match it (or a gvisor release tag no older than it), then verify.
go get gvisor.dev/gvisor@<that-pseudo-version>
go build ./... && go test ./internal/sandboxd/... -count=1
```

- [ ] gvisor pin compared against Tailscale's current pin, and updated or
      consciously held back (record which, and why, in the release notes).
- [ ] Egress tests pass after the bump — the netstack forwarder is what
      enforces default-deny/allowlist (SEC-04).

> **Never pin `gvisor.dev/gvisor@latest`.** gvisor's tip has broken external
> module consumers (a `bridge_test.go` declaring package `bridge` inside
> `pkg/tcpip/stack`). Always pin a release tag or a known-good pseudo-version.

Refs: ADR-010, SEC-04, FR-17.8

Refs: MGIT-48, MGIT-44, MGIT-61.2
