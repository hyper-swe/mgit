# Package Approval Request

**Submitted by:** mgit engineering
**Date:** 2026-06-12
**Status:** Approved — recorded in APPROVED-PACKAGES.md §2a (sandbox helper scope; CGO exception)

---

## Package Information

**Package Name:** github.com/Code-Hex/vz/v3
**Proposed Version:** ≥ 3.1.0 (pin exact version at first import)
**Home Page:** https://github.com/Code-Hex/vz
**License:** MIT

## Purpose in mgit

macOS microVM backend for `mgit-sandboxd` (FR-17.15, ADR-005): Go bindings for
Apple's Virtualization.framework — VM lifecycle, virtio-fs directory sharing
(the worktree mount, FR-17.3), vsock devices, NAT/no-NIC network configuration,
and memory ballooning (NFR-17.4). Native, no kernel extension required, works on
Apple silicon and Intel.

## Alternatives Considered

- **Shelling out to Apple's `container`/`vz` CLI tooling** — out-of-process
  control with no typed API, fragile parsing, and a process-spawning surface.
  Rejected; same rationale as the no-git-binary rule (determinism, audit).
- **Custom cgo bindings to Virtualization.framework** — reimplements exactly
  what vz provides (thousands of lines of Objective-C bridge); unauditable
  in-house. Rejected on Implementation Necessity.
- **Docker Desktop / Lima as the VM layer** — heavyweight daemon dependency,
  shared VM across tasks violates FR-17.1 one-task-one-VM. Rejected.

## CGO Exception (FR-17.16, ADR-005 "CGO containment")

Virtualization.framework is an Objective-C framework; **any** binding requires
CGO. This package is approved **only** for the separate `mgit-sandboxd` helper
binary on darwin. Core `mgit` remains pure-Go and CGO-free; CI enforces
`CGO_ENABLED=0` builds of core. This is the explicit ADR-005 resolution of the
CGO conflict, not a relaxation of the pure-Go policy.

## Evaluation Checklist

- [x] **Pure Go** — **EXCEPTION:** CGO required by the platform framework; confined to mgit-sandboxd (darwin) per FR-17.16.
- [x] **License Approved** — MIT.
- [x] **Actively Maintained** — active repo, tracks new macOS releases.
- [x] **Security (No CVEs)** — govulncheck clean at evaluation date; re-run at pin time.
- [x] **Minimal Dependencies** — small tree (x/sys class); reviewed at pin time.
- [x] **Test Coverage** — upstream CI on macOS runners.
- [x] **No Overlap** — no approved package touches Virtualization.framework.
- [x] **Implementation Necessity** — Objective-C bridge is several thousand lines; not implementable in <100 lines.

## Detailed Justification

macOS is a primary developer platform for mgit's users; FR-17.15 requires a
native backend there. Virtualization.framework is Apple's supported hypervisor
API (the same foundation as Apple's containerization tooling), and vz is the
de-facto Go binding used across the ecosystem. The CGO cost is contained by
ADR-005's helper-binary architecture and is the documented reason that
architecture exists.

## Impact Assessment

**Binary size increase:** ~3–5 MB in mgit-sandboxd (darwin) only; core mgit unchanged.
**Startup time impact:** none for core mgit.
**Security surface:** platform framework bindings; VMM is Apple COTS, assessed per FR-17.30.
**Maintenance burden:** Medium (tracks macOS releases; pinned + re-baselined per FR-17.36).

## Sign-off

Evaluated against all PACKAGE-APPROVAL-PROCESS.md criteria; approved for the
mgit-sandboxd helper scope only, darwin builds only, with the documented CGO
exception.

**Requester:** mgit engineering — **Date:** 2026-06-12
**Refs:** MGIT-11.1.4, FR-17.15, FR-17.16, ADR-005
