# ADR-006: Host-Matching Sandbox Guests; Windows Backend via WCOW

**Status:** Accepted
**Date:** 2026-06-15
**Refs:** ADR-005 (MicroVM Sandbox — this ADR refines its platform model), FR-17.15 (pluggable backend), FR-17.27 (vsock/HvSocket peer binding), FR-17.35 (control/land protocol), FR-17.39 (new — host-matching guest), FR-16 (Agent Worktrees)

---

## Context

ADR-005 established the microVM sandbox and named three native backends — `kvm` (Linux), `vzf` (macOS Virtualization.framework), `hyperv` (Windows) — with an implicit assumption carried over from the Linux/macOS work: **the guest is always Linux** (a pinned kernel + rootfs booted as a microVM). The macOS and Linux backends were built on that basis and shipped (MGIT-11.5.1, MGIT-11.5.2).

Implementing the Windows backend on that assumption — a **Linux** guest on a Windows host (LCOW) — ran into a wall:

- **No supported public API.** The LCOW utility-VM lifecycle lives in `hcsshim`'s `internal/uvm` + `internal/lcow` packages; Go forbids importing them. The only paths are vendoring unsupported internal code (a supply-chain/governance hazard for a safety-critical project) or hand-rolling against the HCS API.
- **Deprecated on the client.** Microsoft deprecated standalone LCOW on Windows 10/11 clients; the modern containerd LCOW path states a platform requirement of **Windows Server 2025 (build 26100)+**.
- **Translation tax.** A Linux guest on a Windows host forces Linux↔Windows path, line-ending, and permission translation of the working tree at the `land` boundary — in tension with FR-17.3 ("identical absolute path, no path translation").

More fundamentally: **mgit targets developers, and a developer builds with the toolchain of the OS they are on.** A Windows developer's `npm`, `python`, `go`, `cargo`, and MSVC all run natively on Windows; they are isolating *Windows* build/test runs, not producing Linux artifacts. The sandbox exists to confine *their* execution, so the guest should match the host. The premise "the guest must be Linux" was a default inherited from the first two backends, not a requirement.

The native, **supported, non-deprecated** Windows isolation primitive is a **Hyper-V-isolated Windows container (WCOW)** — a throwaway Windows guest in its own utility VM, available on Windows 10/11 Pro and Server 2016+, the same mechanism Docker/AKS use.

### Pilot (2026-06-15)

Validated on a Windows 10 Pro 22H2 runner (build 19045) before committing:

- `docker run --isolation=hyperv` booted an `ltsc2019` (build 17763) Windows Server Core container — confirming an older Windows host **can** Hyper-V-isolate a Windows container (version compatibility: host build ≥ container base).
- Inside the isolated container, every tool on the target list compiled and/or ran: **Go 1.23.4**, **Node 20.18.1 + npm** (→ React/Angular), **Python 3.12.7**, **C** (gcc 14.2.0), **C++** (g++ 14.2.0, STL), **Rust 1.96.0** (rustc + cargo, GNU toolchain).

Full results: MGIT-11.5.3 annotations.

## Decision

1. **Sandbox guests are host-matching.** The guest OS family matches the host so the guest runs the developer's own toolchain and the working tree crosses the `land` boundary without translation:
   - **Linux host → Linux microVM** (`kvm`, Firecracker/KVM — ADR-005, shipped).
   - **Windows host → Hyper-V-isolated Windows container (WCOW)** — a *Windows* guest, **not** a Linux guest (not LCOW).
   - **macOS host → Linux microVM** (`vzf`, Virtualization.framework — shipped) as the **documented exception**: macOS cannot host nested macOS guests, and Mac developers overwhelmingly target Linux/containers or cross-compile.

2. **The Windows backend uses HCS-backed WCOW**, driven **host-side and headlessly** via the documented Host Compute System API (`vmcompute.dll` / `computecore.dll`) and/or containerd — **never Docker Desktop** (a GUI/session-bound tool unsuitable for a daemon). The control/land channel is **HvSocket (AF_HYPERV)**, the Windows analog of vsock already anticipated by FR-17.27. The guest runs a **Windows `mgit-guest`** agent (the existing `mgit-guest` is Linux-only, `//go:build linux`).

3. This **supersedes the ADR-005 platform table's Windows row** ("WSL2 utility-VM or WHP-based VMM"). WSL2 is rejected (shared utility VM violates the one-task-one-VM isolation of FR-17.1); LCOW is rejected (internal-only API, client-deprecated, translation tax); QEMU+WHPX is rejected (large attack surface, WHP/reboot, finicky, and ADR-005 already rejected QEMU); Cloud Hypervisor is rejected (Linux-host only, no Windows-host port).

## Consequences

**Positive**
- Supported and non-deprecated on Windows 10/11 Pro — no Server 2025 requirement, no internal-code vendoring.
- Uses the OS-native isolation primitive on every platform.
- Host-matching guests remove the cross-OS translation tax at `land` and keep FR-17.3's "identical path" property intact per platform.

**Negative / costs**
- The Windows backend does **not** reuse the Linux/macOS `microvm.Manager` seam (kernel + rootfs + VMConfig): WCOW is a container + utility-VM model, closer to a Hyper-V-isolated variant of the container backend. A separate manager is expected.
- A **Windows `mgit-guest`** agent and a **prepared, digest-pinned Windows base image** (FR-17.17, FR-17.31) are new artifacts.
- **Version compatibility:** Hyper-V isolation requires the host build ≥ the container base build; a Win10 22H2 host is limited to `ltsc2019`, while `ltsc2022` needs a Win11 / Server 2022 host. Dev/test of this backend should target **Windows 11 Pro / Server 2022**.
- The SANDBOX-IMAGES register (FR-17.31) gains Windows base images alongside the Linux rootfs images.

**Accepted trade-off**
- A task's sandbox environment differs by host OS. This is acceptable: the developer's toolchain already differs by host, and the `land` boundary (dual-hash per ADR-002, task-ID binding, host-anchored attestation) is OS-agnostic, so provenance and integrity guarantees are identical regardless of guest OS.

## Status of work

- Decision recorded and validated by pilot (2026-06-15). Implementation is **deferred** behind a dedicated epic (MGIT-12 — "Windows-native sandbox backend (WCOW)"), to be picked up when there is developer demand for Windows-host support. Until then, Windows hosts are served by the audited reduced-isolation container fallback (FR-17, `--backend container --acknowledge-reduced-isolation`).
