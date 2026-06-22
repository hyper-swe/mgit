# Package Approval Request

**Submitted by:** mgit engineering
**Date:** 2026-06-22
**Status:** Approved — recorded in APPROVED-PACKAGES.md §2a (sandbox helper scope)

---

## Package Information

**Package Name:** golang.org/x/net/dns/dnsmessage
**Proposed Version:** ≥ v0.55.0 (the version already pinned in go.mod; x/net is
currently an indirect dependency — this promotes the `dns/dnsmessage`
subpackage to a direct import)
**Home Page:** https://pkg.go.dev/golang.org/x/net/dns/dnsmessage
**License:** BSD-3-Clause (the Go project license)

## Purpose in mgit

The host-side restricted DNS server for `allowlist`-mode sandboxes (FR-17.8,
SEC-04, SEC-07): it answers the guest's DNS queries on the per-sandbox gateway,
resolving **only** allowlisted names via the host (`egress.Resolver`), pinning
the returned IPs (so the egress proxy admits the subsequent connection), and
refusing everything else. The DNS queries originate from the **hostile guest**,
so the wire-format parser is security-critical hostile-input surface — exactly
the class the FR-17 audits require to be robust (land-parser fuzzing ethos).

`dns/dnsmessage` is the low-level DNS wire codec written and maintained by the
Go team; it is the same parser the Go standard library's own `net` resolver
uses internally. Using it avoids hand-rolling a DNS packet parser (label
compression loops, overlong names, truncation, pointer bombs) for hostile
input — the safer choice for a security boundary.

## Alternatives Considered

- **Hand-rolled minimal DNS parser (~150 lines, no new dep)** — feasible, but
  re-derives a hostile-input parser the Go team already maintains and fuzzes.
  For a guest-facing security boundary, a vetted codec is the lower-risk option.
  Rejected: parser risk outweighs the dependency.
- **github.com/miekg/dns** — full-featured DNS library, far larger surface than
  needed (we only parse a question and build an A/AAAA answer) and a new
  third-party dependency. Rejected: oversized; dnsmessage is sufficient.
- **No DNS server; require the guest to send hostnames to the proxy** — works
  only for clients that defer resolution to the proxy; ordinary
  resolve-then-connect clients (getaddrinfo + connect-by-IP) need a resolver.
  Rejected: breaks standard tooling inside the guest.

## Evaluation Checklist

- [x] **Pure Go (no CGO)** — pure Go; no C bindings.
- [x] **Maintained by the Go team** — golang.org/x/net, same trust class as the
  already-approved golang.org/x/sync, golang.org/x/crypto, golang.org/x/sys.
- [x] **Already in the module graph** — present as an indirect dependency
  (v0.55.0); this only promotes one subpackage to a direct import. No new module
  is added to go.sum.
- [x] **Sandbox-confined** — imported only from the `internal/sandboxd` tree
  (the DNS server), never from core `mgit`. §2a scope.
- [x] **Minimal surface** — only `dns/dnsmessage` (a codec); no network or
  server code from the package is used (the UDP/TCP listener is mgit's own).
- [x] **License compatible** — BSD-3-Clause (Go project).

## Decision

Approved for `mgit-sandboxd` scope only (§2a), exclusively for the host-side
restricted DNS server. Core `mgit` must not import it (enforced by the
import-confinement test). Refs: FR-17.8, SEC-04, SEC-07, MGIT-11.7.3.
