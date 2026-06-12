# Package Approval Request

**Submitted by:** mgit engineering
**Date:** 2026-06-12
**Status:** Approved — recorded in APPROVED-PACKAGES.md §2a (sandbox helper scope)

---

## Package Information

**Package Name:** github.com/mdlayher/vsock
**Proposed Version:** ≥ 1.2.1 (pin exact version at first import)
**Home Page:** https://github.com/mdlayher/vsock
**License:** MIT

## Purpose in mgit

vsock (virtio socket) transport for the sandbox control plane and land protocol
(FR-17.5, FR-17.27, FR-17.35): host↔guest communication with **no NIC and no IP
stack**, the property the ADR-005 design depends on for `none`-mode sandboxes
and for keeping the land channel off the network entirely. Provides
`net.Conn`/`net.Listener` semantics over AF_VSOCK, including the connection CID
needed for FR-17.27 peer binding.

## Alternatives Considered

- **Raw AF_VSOCK via golang.org/x/sys** — implementable (~200–400 lines with
  listener semantics, CID handling, platform quirks, tests) but re-derives
  exactly what this small, focused package provides and is easy to get subtly
  wrong (non-blocking accept, EINTR, CID edge cases). Rejected: the package is
  the auditable encoding of those ~400 lines.
- **TCP over a NAT interface** — requires attaching a NIC, destroying the
  `none`-mode guarantee and widening the land-channel attack surface. Rejected
  outright (contradicts FR-17.7).
- **Serial/console transport** — no framing, no concurrency, poor throughput
  for object transfer (FR-17.35 ceilings). Rejected.

## Evaluation Checklist

- [x] **Pure Go (no CGO)** — pure Go syscall usage.
- [x] **License Approved** — MIT.
- [x] **Actively Maintained** — active maintainer with a strong low-level networking track record.
- [x] **Security (No CVEs)** — govulncheck clean at evaluation date; re-run at pin time.
- [x] **Minimal Dependencies** — tiny tree (mdlayher/socket + x/sys); well under the 10-dep red flag.
- [x] **Test Coverage** — upstream tests + CI.
- [x] **No Overlap** — no approved package provides AF_VSOCK.
- [x] **Implementation Necessity** — borderline by line count (~400), approved for correctness risk: the land protocol's security properties (FR-17.24, FR-17.27) sit directly on this transport.

## Detailed Justification

The quarantine-then-land design (FR-17.4/17.5) requires a host↔guest channel
that exists independently of any network policy, so that `none`-mode sandboxes
still commit and land. AF_VSOCK is that channel on KVM, and
Virtualization.framework exposes it guest-side via virtio sockets; Hyper-V
provides the equivalent hypervisor socket family (AF_HYPERV, addressed by VM
GUID rather than CID), which is covered by the hcsshim/Windows backend at
implementation time — not by this package (hence the linux-only constraint in
§2a). The peer addressing this package surfaces is the enforcement primitive
for SEC-10/FR-17.27 on AF_VSOCK backends.

## Impact Assessment

**Binary size increase:** <1 MB in mgit-sandboxd; core mgit unchanged.
**Startup time impact:** none.
**Security surface:** the land-protocol transport — intentionally so; hardened per FR-17.24/17.27/17.35.
**Maintenance burden:** Low.


## Known-Vulnerability Check (2026-06-12, pre-import)

GitHub Advisory Database (ecosystem:Go) and OSV: **no advisories** for
`github.com/mdlayher/vsock`.

## Sign-off

Evaluated against all PACKAGE-APPROVAL-PROCESS.md criteria; approved for the
mgit-sandboxd helper scope only.

**Requester:** mgit engineering — **Date:** 2026-06-12
**Refs:** MGIT-11.1.4, FR-17.5, FR-17.27, FR-17.35, ADR-005
