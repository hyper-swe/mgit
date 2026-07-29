# mgit release checklist

Releases are **owner-triggered**. This is the gate list: what CI proves
automatically, and what a human must verify live before publishing — because
the sandbox path needs virtualization that hosted CI runners do not have.

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
  - **sandbox-posture** — the gate logic; SKIPs on hosted runners (no
    virtualization).

A regression like "mgit-sandboxd missing from the archives" or "an MCP tool
returns placeholder text" fails these gates before a user ever sees it.

## Mandatory manual live passes (CI cannot run these)

The per-task microVM sandbox is the headline differentiator and cannot be
exercised on a GitHub-hosted runner. **Before publishing, run at least one live
sandbox pass per supported platform** and record the result on the release:

- [ ] **Linux (KVM)** — on a KVM-capable host (the Linux runner or a nested-virt
      VM), with `mgit-sandboxd` from the release artifact set and a provisioned
      guest image:
      ```
      MGIT_GUEST_IMAGE=<image> bash scripts/e2e/sandbox_posture.sh <bindir>
      ```
      must print `SANDBOX POSTURE E2E: PASS (live)`.
- [ ] **macOS (arm64)** — on an Apple Silicon host, with the entitlement-signed
      `mgit-sandboxd` from the release archive and a guest image, the same
      script must print `PASS (live)`.

> Never refer to the Linux KVM host by its LAN IP in the repo, CI logs, or the
> release notes — call it "the Linux runner".

## Publish steps (owner)

1. Ensure `main` is green and the CHANGELOG `[Unreleased]` section is ready.
2. Tag and push: `git tag vX.Y.Z && git push origin vX.Y.Z`. This triggers
   `release.yml` (preflight → e2e gate → macOS release build+sign → GoReleaser).
3. Reconcile the Homebrew tap so the formula installs **both** binaries — apply
   the change in `docs/release/homebrew-tap-formula.md` to the separate
   `hyper-swe/homebrew-tap` repo (must not touch the `mtix` formula). MGIT-44.
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
7. **libkrun networking capability check** (clean machine, sandbox builds only).
   libkrun gates its net-device API behind an opt-in build flag, and mgit
   requires an explicit NIC in every mode — without one libkrun falls back to
   TSI and the guest gets full host egress, so a libkrun built without
   networking cannot host a sandbox at all. The `libkrun/krun` tap passes
   `NET=1` explicitly today, but that is a THIRD-PARTY build flag: if the
   formula ever drops it, every Mac install breaks. Verify per release:
   ```
   brew tap libkrun/krun && brew install libkrun
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
