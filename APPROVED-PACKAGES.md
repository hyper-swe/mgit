# APPROVED-PACKAGES.md
## mgit Package Approval Registry

**Last Updated:** 2026-06-12
**Go Version Requirement:** 1.23+
**Status:** Active

---

## 1. Introduction

This document maintains the **exclusive list of approved dependencies** for the mgit (micro git) project. mgit is a safety-critical micro version control system designed for LLM coding agents.

### Policy

- **Only packages listed below may be imported** into mgit codebase.
- **Violation of this policy results in code rejection** during review and CI/CD pipeline checks.
- Using an unapproved package without prior approval = automatic code rejection.
- All packages approved for **core mgit** must meet the **Pure Go requirement** (no CGO) to ensure single-binary deployment across all platforms. The sole exception is §2a: packages approved exclusively for the separate `mgit-sandboxd` helper binary (FR-17.16, ADR-005), which may require CGO and are **never importable from core mgit**.

### Safety-Critical Constraint

mgit supports LLM coding agents in critical workflows. Dependencies are vetted for:
- Security posture (no known CVEs)
- Maintenance status (active, responsive maintainers)
- Pure Go compilation (no C bindings) — core mgit; see §2a for the sandboxd-only exception
- Minimal transitive dependencies
- License compatibility (open-source only)

---

## 2. Approved Dependencies (Core Table)

| Package | Purpose | Min Version | Justification |
|---------|---------|-------------|---------------|
| `github.com/go-git/go-git/v5` | Pure Go git implementation (plumbing layer) | 5.13.0 | **Core git engine.** Decision recorded in ADR-001. Pure Go with no CGO, full access to git plumbing API. Supports object database, index manipulation, reference updates, and low-level pack operations. No external git binary required. **Note:** go-git v6 is planned for future adoption (see ADR-004) once it meets maturity criteria. v6 is NOT approved for use until formally evaluated and added to this table. |
| `modernc.org/sqlite` | Pure Go SQLite3 driver | 1.35.0 | **Task-commit indexing.** Pure Go (no CGO), production-proven in large-scale deployments. Used for audit log storage, commit metadata indexing, and squash operation tracking. Supports transactions for atomicity. |
| `github.com/spf13/cobra` | CLI framework for command structure | 1.8.1 | **Industry standard CLI toolkit.** Used by kubectl, Hugo, Docker, and 1000+ projects. Provides subcommand routing, flag parsing, help generation, and shell completions for mgit's 15+ commands (commit, amend, squash, reset, etc.). |
| `github.com/labstack/echo/v4` | Lightweight HTTP web framework | 4.13.4 | **REST API server.** Minimal dependency tree, high performance. Used for health check endpoint, commit query API, squash operation triggers, and webhook receivers. Zero-copy middleware support. |
| `github.com/oklog/ulid/v2` | ULID ID generation | 2.1.0 | **Distributed, time-sortable unique identifiers.** Used for audit entry IDs, operation logs, and distributed task tracking. Lexicographic ordering ensures chronological consistency without clock synchronization. |
| `github.com/stretchr/testify` | Enhanced test assertion library | 1.10.0 | **Test readability.** `require` package for fatal assertions, `assert` package for non-fatal. Dramatically improves test failure diagnostics with readable output. |
| `golang.org/x/sync` | Additional synchronization primitives | Latest | **Goroutine lifecycle management.** `errgroup.Group` used for background worker coordination, error collection, and graceful cancellation. Ensures all goroutines complete before shutdown. |
| `golang.org/x/crypto` | Extended cryptography library | Latest | **Ed25519 commit signing (optional feature).** Only imported if signing is enabled. For hashing operations, prefer `crypto/sha256` from stdlib. |
| `github.com/mark3labs/mcp-go` | Model Context Protocol (MCP) SDK | Latest | **MCP server implementation.** Same library as mtix (NFR-4.6). Provides JSON-RPC stdio and SSE transports for LLM agent integration. Pure Go, no CGO. |
| `github.com/go-git/go-billy/v5` | Filesystem abstraction used by go-git | 5.8.0 | **go-git companion.** Required by the fsync-aware storage wrapper (MGIT-2.2.7) and worktree filesystem plumbing. Same org and supply-chain posture as go-git itself. Pure Go. |
| `golang.org/x/sys` | Low-level OS primitives | Latest | **Process-level file locking** (flock/LockFileEx, MGIT-10.1) and syscall-level primitives the stdlib does not expose. Maintained by the Go team; already a transitive dependency of approved packages. |
| `log/slog` | Structured logging (stdlib) | Go 1.22+ | **PREFERRED production logger.** Zero-allocation structured logging with JSON and text output modes. Composable handlers. Supports log levels and context propagation. |

---

## 2a. Approved for `mgit-sandboxd` Only (Sandbox Helper Scope)

The FR-17 sandbox backends live in the separate `mgit-sandboxd` helper binary
(FR-17.16, ADR-005 "CGO containment"). The packages below are approved
**exclusively** for that binary — importing any of them from core `mgit`
(`cmd/mgit`, `internal/**` outside the sandboxd tree) is a policy violation.
Core `mgit` remains pure-Go and CGO-free; CI enforces `CGO_ENABLED=0` core
builds. Full evaluations: `pkg-approvals/approved/`. Refs: MGIT-11.1.4.

| Package | Purpose | Min Version | Platform / Constraint | Justification |
|---------|---------|-------------|----------------------|---------------|
| `github.com/firecracker-microvm/firecracker-go-sdk` | Linux KVM microVM control (Firecracker-class VMM) | 1.0.0 | linux only; pure Go (VMM binary is COTS, assessed per FR-17.30/31) | **Linux backend (FR-17.15).** Battle-tested lineage (AWS Lambda, E2B) for running untrusted code. Rejected alternatives: libkrun (CGO in control path, immature Go bindings), hand-rolled Cloud Hypervisor client (~1,500 lines), QEMU/libvirt (surface + boot latency vs NFR-17.2). |
| `github.com/Code-Hex/vz/v3` | macOS Virtualization.framework bindings | 3.1.0 | darwin only; **CGO — confined to mgit-sandboxd, never core** | **macOS backend (FR-17.15).** Native hypervisor API, no kext, Apple-silicon + Intel; provides virtio-fs, vsock, ballooning (NFR-17.4). CGO is unavoidable for any Virtualization.framework binding — this is the documented FR-17.16 exception. Rejected: CLI shell-outs (non-deterministic), custom ObjC bridge (unauditable), Docker Desktop/Lima (shared VM violates FR-17.1). |
| `github.com/Microsoft/hcsshim` | Windows Hyper-V utility-VM control (HCS) | 0.12.0 | windows only; pure Go | **Windows backend (FR-17.15).** Microsoft's own Go interface to HCS — the Docker/containerd Hyper-V isolation code path. Rejected: raw WHP via x/sys (in-house VMM, multi-thousand lines), PowerShell/WMI shell-out, WSL2 shared VM (violates FR-17.1, SEC-08). |
| `github.com/mdlayher/vsock` | AF_VSOCK transport (control plane + land protocol) | 1.2.1 | linux host/guest; pure Go; tiny dep tree | **Land/control channel (FR-17.5, FR-17.27, FR-17.35).** Network-independent host↔guest transport — required for `none`-mode sandboxes; exposes connection CID for SEC-10 peer binding. Rejected: raw AF_VSOCK via x/sys (~400 error-prone lines), TCP-over-NIC (destroys `none` mode), serial transport (no framing). |
| `github.com/sirupsen/logrus` | Logging adapter for `firecracker-go-sdk` only | 1.9.3 | sandbox-confined; **rejected for core (§4) — use `log/slog`** | **Not a logging choice.** `firecracker-go-sdk`'s `WithLogger` API is typed on `*logrus.Entry`, so the Linux backend imports logrus solely to hand the SDK a silenced (discard) logger and keep its diagnostics off the daemon's streams. Never imported outside the sandbox tree (enforced by `TestImports_SandboxDepsConfinedToSandboxd`); core mgit logging remains `log/slog`. |
| `golang.org/x/net` | DNS wire codec (`dns/dnsmessage`) for the host-side restricted resolver | 0.55.0 | sandbox-confined direct import; pure Go; already an indirect dep | **Host DNS server (FR-17.8, SEC-04, SEC-07).** Only the `dns/dnsmessage` subpackage is imported, and only from the `internal/sandboxd` tree: the `allowlist`-mode gateway DNS server parses the hostile guest's queries with the Go team's own codec (the one `net.Resolver` uses internally) rather than a hand-rolled hostile-input parser, then resolves only allowlisted names host-side and pins the result. Same trust class as the approved `golang.org/x/{sync,crypto,sys}`; the module is also a common transitive of other approved deps. Rejected: hand-rolled parser (hostile-input parser risk on a security boundary), `miekg/dns` (oversized new dep). |

**Transitive note (FR-17.30):** `firecracker-go-sdk` pulls `github.com/pkg/errors`
(also §4-rejected) as an indirect dependency. mgit never imports it — the
import-confinement test forbids it in core, so it exists only inside the
SDK's own call graph. No remediation is possible without forking the SDK; it is
accepted as a sandbox-scoped transitive per ADR-005 criterion 2.

**Image signature verification (FR-17.29): no new dependency.** Signatures use
stdlib `crypto/ed25519` (+ approved `golang.org/x/crypto` if needed for key
formats) as detached signatures over image digests in `images.lock`.
`sigstore/cosign`-class tooling was evaluated and **rejected** for v1: its
transitive dependency tree (>40 modules) fails criterion 2.5 by an order of
magnitude. Cosign interop may be re-proposed separately if registry-based
distribution is ever adopted.

---

## 3. Standard Library Preferences

The following stdlib packages are preferred over third-party alternatives:

### Cryptography & Hashing
- `crypto/sha256` — Content-addressable storage (objects, blobs)
- `crypto/sha1` — Git object IDs (compatibility with git protocol)
- Prefer stdlib over `golang.org/x/crypto` unless Ed25519 signing required

### Database & Storage
- `database/sql` — Standard SQL interface; use with `modernc.org/sqlite` driver
- Parameterized queries (? placeholders) for SQL injection prevention
- Transactions via sql.Tx for atomic multi-statement operations

### Serialization
- `encoding/json` — JSON serialization for API responses and config files
- `encoding/base64` — For pack data encoding
- Standard `json.Unmarshal` and `json.Marshal`

### HTTP & Networking
- `net/http` — HTTP primitives; Echo wraps this
- `net/url` — URL parsing and query parameters
- `io/ioutil.ReadAll` (legacy) replaced with `io.ReadAll`

### Filesystem & Path Operations
- `os` — File operations (Open, Create, Remove, Stat)
- `path/filepath` — Platform-safe path joining and manipulation
- `io/fs` — Filesystem interfaces and WalkDir

### Concurrency & Control Flow
- `context` — Request cancellation, timeouts, and value propagation
- `sync` — Mutex, RWMutex, WaitGroup for goroutine coordination
- Use `golang.org/x/sync.errgroup` for error collection in parallel tasks

### Error Handling
- `fmt.Errorf` with `%w` for error wrapping
- `errors.New` for simple error definitions
- `errors.Is` and `errors.As` for error type checking
- Avoid `github.com/pkg/errors` (deprecated)

### Testing
- `testing.T` and `testing.B` for test and benchmark functions
- `testing.TB` interface for reusable test helpers
- Combine with `github.com/stretchr/testify` for readable assertions

### I/O Operations
- `io.Reader` and `io.Writer` for streaming interfaces
- `io.Copy` for efficient data transfer
- `bufio.Scanner` for line-oriented input
- `io/ioutil.ReadAll` → `io.ReadAll` (Go 1.16+)

---

## 4. Explicitly Rejected Packages

These packages are **forbidden** in mgit. Do not use them under any circumstances:

| Package | Reason | Alternative |
|---------|--------|-------------|
| `github.com/mattn/go-sqlite3` | Requires CGO — violates single-binary requirement. Produces platform-specific binaries. | Use `modernc.org/sqlite` |
| `gorm.io/gorm` | ORM abstracts SQL, hides parameterized queries, difficult to audit for injection vulnerabilities. | Use `database/sql` directly |
| `github.com/jmoiron/sqlx` | Unnecessary abstraction over `database/sql`. Adds complexity without safety benefit. | Use `database/sql` |
| `github.com/sirupsen/logrus` | Deprecated package. Superseded by `log/slog`. Frozen at v1.x with no stdlib integration. | Use `log/slog` |
| `github.com/pkg/errors` | Deprecated. Stdlib `fmt.Errorf` with `%w` replaces all functionality. | Use `fmt.Errorf` + `errors.Is/As` |
| `github.com/libgit2/git2go` | Requires CGO (wraps libgit2 C library). Incompatible with single-binary deployment. | Use `github.com/go-git/go-git/v5` |
| External git binary (via `exec.Command`) | Out-of-process execution creates deployment friction, platform dependencies, security surface. | Use `github.com/go-git/go-git/v5` plumbing |
| `github.com/gin-gonic/gin` | mgit standardized on Echo for HTTP framework. Using both creates inconsistency. | Use `github.com/labstack/echo/v4` |
| `github.com/spf13/viper` | Configuration complexity not justified for mgit's minimal config needs. YAML parsed via stdlib. | Use `encoding/yaml` from stdlib alternatives or simple config struct |
| Any ORM except direct database/sql | ORM usage contradicts safety-critical requirements for auditable, parameterized SQL. | Use `database/sql` with explicit queries |
| `gopkg.in/yaml.v3` (external) | Modern stdlib YAML support adequate. External version adds unnecessary dependency. | Use stdlib when available; evaluate necessity |

---

## 5. Dependency Versioning Rules

All dependencies must follow strict versioning discipline:

### go.mod Management
- All versions **must be pinned** in `go.mod` (no `latest` or version ranges)
- Format: `require package/name v1.2.3` (exact version)
- Transitive dependencies locked via `go.sum`
- `go.sum` committed to repository and verified in CI

### Security & Maintenance
- **govulncheck** run on every release before publishing
  ```bash
  go install golang.org/x/vuln/cmd/govulncheck@latest
  govulncheck ./...
  ```
- No known CVEs in any dependency (checked against NVD)
- Deprecated packages rejected immediately

### Constraint on Modifications
- **No replace directives** in `go.mod` (except isolated development branches)
- Replace directives indicate dependency problems; either fix or change package
- Transitive dependencies must be reviewed for CGO contamination
  ```bash
  go list -json ./... | jq '.Deps' | xargs -I {} go list -json {} | grep -i cgo
  ```

### Update Procedure
1. Identify minimum acceptable version
2. Test against that version (not latest)
3. Pin in go.mod
4. Run govulncheck and test suite
5. Commit go.mod and go.sum together

---

## 6. Version Compatibility

### Go Version Requirement
- **Minimum Go version: 1.23**
- All dependencies must support Go 1.23+
- Use `go.mod` version: `go 1.23`
- No deprecated API usage (checked with `go vet`)

### Dependency Compatibility
- Dependencies must compile on Go 1.23+
- No beta or RC versions of Go
- LTS versions preferred (1.20 LTS, 1.24 future LTS)
- Deprecated package functions flagged in code review

### Build Verification
```bash
# Verify all dependencies compile for Go 1.23+
go build -v ./...
go test -v ./...

# Check for deprecated API usage
go vet ./...

# Verify no CGO contamination
CGO_ENABLED=0 go build -v ./...
```

---

## 7. Adding New Dependencies

**Do not add a new dependency without prior approval.**

Process:
1. Stop work immediately
2. Create a PACKAGE-APPROVAL-REQUEST.md document (see PACKAGE-APPROVAL-PROCESS.md)
3. Submit for review
4. Wait for explicit approval
5. Update APPROVED-PACKAGES.md upon approval
6. Add to go.mod and commit both files together

Any commit adding an unapproved dependency will be rejected.

---

## 8. Approval Authority

Package approvals decided by:
- Safety-critical compliance review
- Go build verification (no CGO)
- License compliance check
- Security assessment (govulncheck clean)
- Architectural necessity evaluation

---

## 9. Revision History

| Date | Change | Rationale |
|------|--------|-----------|
| 2026-03-09 | Initial registry created | mgit project launch |
| 2026-03-12 | Noted go-git v6 upgrade path (ADR-004) | Pluggable worktree strategy; v6 not yet approved for use |
| 2026-06-12 | Added §2a sandbox-helper scope: firecracker-go-sdk, Code-Hex/vz/v3 (CGO exception), Microsoft/hcsshim, mdlayher/vsock; recorded stdlib-Ed25519 decision for FR-17.29 (sigstore rejected) | FR-17 backend dependencies per ADR-005 adoption criterion 2 (MGIT-11.1.4) |
| 2026-06-12 | Registered already-in-use direct deps go-billy/v5 and golang.org/x/sys in core table | Registry was stale vs go.mod; caught by TestGoMod_NoUnapprovedDeps |

---

## Footer

This registry is enforced via CI/CD hooks and code review. Violations will result in:
1. Automatic CI rejection
2. Code review rejection
3. Blocking of pull request merge

For questions or exceptions, submit a formal PACKAGE-APPROVAL-REQUEST.
