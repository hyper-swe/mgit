# Package Approval Request

**Submitted by:** mgit engineering
**Date:** 2026-06-12
**Status:** Approved — recorded in APPROVED-PACKAGES.md §2a (sandbox helper scope)

---

## Package Information

**Package Name:** github.com/firecracker-microvm/firecracker-go-sdk
**Proposed Version:** ≥ 1.0.0 (pin exact version at first import)
**Home Page:** https://github.com/firecracker-microvm/firecracker-go-sdk
**License:** Apache-2.0

## Purpose in mgit

Linux microVM backend for `mgit-sandboxd` (FR-17.15, ADR-005). The SDK drives the
Firecracker VMM over its API socket to launch, configure, and tear down KVM-backed
microVMs: machine config, drives (read-only rootfs + COW overlay), network
interfaces (or none, per FR-17.7), and vsock devices. The Firecracker VMM binary
itself is COTS, digest-pinned, and assessed per FR-17.30/FR-17.31.

## Alternatives Considered

- **libkrun (containers/libkrun)** — C library; Go usage requires CGO in the VMM
  control path and the Go bindings are immature. Rejected: keeps CGO out of even
  the helper where a pure-Go alternative exists, and the Firecracker lineage
  (AWS Lambda, E2B) is more battle-tested for the exact untrusted-code use case.
- **Cloud Hypervisor REST control (hand-rolled client)** — viable but means
  writing and maintaining our own VMM client (~1,500+ lines incl. tests) against
  a less stable API surface. Rejected on Implementation Necessity grounds in
  reverse: the SDK is the auditable, maintained encoding of that client.
- **QEMU/libvirt** — far larger attack and dependency surface; boot latency
  incompatible with NFR-17.2. Rejected.

## Evaluation Checklist

- [x] **Pure Go (no CGO)** — SDK is pure Go; talks to the VMM over a unix socket.
- [x] **License Approved** — Apache-2.0.
- [x] **Actively Maintained** — maintained under the firecracker-microvm org.
- [x] **Security (No CVEs)** — govulncheck clean at evaluation date; re-run at pin time.
- [x] **Minimal Dependencies** — moderate tree (containerd net helpers); reviewed at pin time; helper-binary scope contains the blast radius.
- [x] **Test Coverage** — CI on the upstream repo; integration-tested against the VMM.
- [x] **No Overlap** — no approved package provides VMM lifecycle control.
- [x] **Implementation Necessity** — a correct VMM API client + jailer integration is well over 100 lines (estimate 1,500+).

## Known Conflicts (documented exceptions)

1. **Maintenance cadence.** The SDK's release cadence is slow (v1.0.0 era);
   activity is sparse relative to the other §2a approvals. Mitigations: pin
   exact version at first import, re-baseline under FR-17.36 change control,
   and re-evaluate against Cloud Hypervisor bindings at the MGIT-11.13.2 spike.
2. **logrus API surface.** The SDK's public API accepts `*logrus.Entry`;
   `sirupsen/logrus` is in mgit's Explicitly Rejected table (archived, prefer
   slog). Exception: `mgit-sandboxd` MAY depend on logrus **solely as the
   SDK's logging adapter** (an slog→logrus bridge confined to the sandboxd
   binary). logrus remains forbidden in core mgit and must not be used for
   sandboxd's own logging.

## Detailed Justification

ADR-005 selects hardware-virtualized microVMs as the only acceptable isolation
boundary for executing untrusted agent commands (chroot and OS containers are
explicitly rejected in the ADR). On Linux the Firecracker-class VMM is the
industry-standard mechanism for this exact threat model. The SDK is the
maintained, auditable control plane for it. Confinement: imported **only** by
`mgit-sandboxd` (FR-17.16); core `mgit` never links it.

## Impact Assessment

**Binary size increase:** ~5–8 MB in mgit-sandboxd only; core mgit unchanged.
**Startup time impact:** none for core mgit; sandboxd is socket-activated (NFR-17.6).
**Security surface:** VMM control plane; mitigated by FR-17.34 IPC auth + FR-17.30 COTS assessment.
**Maintenance burden:** Medium (tracks Firecracker releases; pinned + re-baselined per FR-17.36).

## Sign-off

Evaluated against all PACKAGE-APPROVAL-PROCESS.md criteria; approved for the
mgit-sandboxd helper scope only, Linux builds only.

**Requester:** mgit engineering — **Date:** 2026-06-12
**Refs:** MGIT-11.1.4, FR-17.15, FR-17.16, ADR-005
