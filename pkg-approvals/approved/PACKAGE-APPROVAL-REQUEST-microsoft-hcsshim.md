# Package Approval Request

**Submitted by:** mgit engineering
**Date:** 2026-06-12
**Status:** Approved — recorded in APPROVED-PACKAGES.md §2a (sandbox helper scope)

---

## Package Information

**Package Name:** github.com/Microsoft/hcsshim
**Proposed Version:** ≥ 0.12.0 (pin exact version at first import)
**Home Page:** https://github.com/microsoft/hcsshim
**License:** MIT

## Purpose in mgit

Windows microVM backend for `mgit-sandboxd` (FR-17.15, ADR-005): Microsoft's
maintained Go interface to the Host Compute Service (HCS) / Hyper-V platform,
used to create utility VMs with Hyper-V isolation, attach scratch/overlay
storage, and manage VM lifecycle. This is the FR-17.15 "Hyper-V/WHP" mechanism.

## Alternatives Considered

- **Raw Windows Hypervisor Platform (WHP) syscalls via golang.org/x/sys** —
  building a VMM from WHP primitives in-house is a multi-thousand-line project
  (device models, boot, virtio). Rejected on scope and auditability.
- **PowerShell / Hyper-V WMI shell-out** — out-of-process, unparseable,
  non-deterministic control surface. Rejected; same rationale as the
  no-git-binary rule.
- **WSL2 utility VM reuse** — shares one kernel/VM across tasks, violating
  FR-17.1 one-task-one-VM and SEC-08 cleanliness. Rejected as primary
  (documented fallback consideration only).

## Evaluation Checklist

- [x] **Pure Go (no CGO)** — pure Go; drives HCS via Windows syscalls (x/sys), no C toolchain.
- [x] **License Approved** — MIT.
- [x] **Actively Maintained** — maintained by Microsoft (containerd/Windows containers foundation).
- [x] **Security (No CVEs)** — govulncheck clean at evaluation date; re-run at pin time.
- [x] **Minimal Dependencies** — moderate tree (containerd ecosystem); reviewed at pin time; helper-binary scope contains it.
- [x] **Test Coverage** — Microsoft CI; production-proven under Docker/containerd on Windows.
- [x] **No Overlap** — no approved package touches Hyper-V/HCS.
- [x] **Implementation Necessity** — HCS schema + lifecycle handling far exceeds 100 lines.

## Detailed Justification

FR-17.15 requires a Windows backend. HCS is the supported Windows API for
Hyper-V-isolated utility VMs, and hcsshim is Microsoft's own Go binding —
the same code path Docker and containerd use for Hyper-V isolation, which is
the strongest available maintenance and hardening signal. Confined to
`mgit-sandboxd` (windows builds); requires the Hyper-V platform feature, with
the reduced-isolation container fallback (FR-17.15) for hosts without it.

## Impact Assessment

**Binary size increase:** ~6–10 MB in mgit-sandboxd (windows) only; core mgit unchanged.
**Startup time impact:** none for core mgit.
**Security surface:** HCS control plane; VMM is Windows COTS, assessed per FR-17.30.
**Maintenance burden:** Medium (pinned + re-baselined per FR-17.36).

## Sign-off

Evaluated against all PACKAGE-APPROVAL-PROCESS.md criteria; approved for the
mgit-sandboxd helper scope only, windows builds only.

**Requester:** mgit engineering — **Date:** 2026-06-12
**Refs:** MGIT-11.1.4, FR-17.15, FR-17.16, ADR-005
