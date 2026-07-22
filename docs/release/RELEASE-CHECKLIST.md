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
5. **Publish the guest-image bundle** so `mgit sandbox image install` works with
   no `--from` (sandbox-active out of the box, MGIT-61.2):
   ```
   scripts/sandbox-image/publish.sh out/publish        # builds all platform bundles + checksums
   gh release upload <tag> out/publish/*               # attach manifest.json + kernels + rootfs + checksums.txt
   ```
   The install default resolves to the latest release's assets. Then verify on a
   clean host: `mgit sandbox image install` → `mgit run --sandbox -- echo ok`.
   (The vz kernel build needs docker; run `publish.sh` on a machine with it.)
6. Post-publish smoke: `brew install hyper-swe/tap/mgit` on a clean machine and
   confirm `command -v mgit && command -v mgit-sandboxd`.

Refs: MGIT-48, MGIT-44, MGIT-61.2
