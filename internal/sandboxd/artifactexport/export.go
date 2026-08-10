// Package artifactexport is the guest->host artifact bridge: it copies a
// HOST-NAMED path out of a sandbox's staged worktree to a HOST-NAMED
// destination, enforcing every containment invariant host-side before a single
// byte is written.
//
// It is the outbound twin of internal/sandboxd/staging (which delivers the
// worktree inbound) and reuses staging's symlink-escape check rather than
// re-deriving it, so both directions are governed by one implementation.
//
// The guest is the hostile party and this is a guest->host WRITE path, so the
// design is deliberately narrow (ADR-011, MGIT-73):
//
//   - The HOST names both the source (a worktree-relative path) and the
//     destination. A guest-supplied destination would be a host-filesystem
//     write primitive; there is no code path by which the guest names either.
//   - The guest never participates. On the virtiofs backends the guest's
//     worktree IS a host directory, so an export is a host-side READ of the
//     staged tree — the guest gets no say in what leaves and cannot observe
//     or interpose on the transfer.
//   - Every entry is validated BEFORE any host write: relative paths only, no
//     "..", no absolute paths, no symlink or hardlink leaving the exported
//     subtree, no irregular files, and never the sandbox's private mgit store
//     (committed objects cross only through the verified land airlock).
//   - Collisions are REFUSED. An export never overwrites, merges into, or
//     deletes anything that already exists at the destination.
//   - Size and file-count limits bound the transfer, so an export cannot fill
//     the host disk (T7).
//   - The artifact lands atomically with a provenance sidecar naming the
//     sandbox, task, base image digest and per-file hashes (the MGIT-61.15
//     attestation pattern applied to files): a node_modules tree in a host
//     cache with no record of which sandbox produced it is a supply-chain
//     artifact of unknown origin.
//
// It fails closed: any refusal leaves the host filesystem exactly as it was.
// Pure Go, host-only. Refs: MGIT-73, SEC-03, SEC-10, FR-17.3, ADR-011
package artifactexport

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"
)

// Export refusals. Each names one containment rule so a caller (and an
// operator reading an audit line) can tell WHY an export was denied, never
// merely that it failed. An escaping symlink surfaces staging.ErrSymlinkEscape
// unchanged — the same error the inbound direction raises. Refs: MGIT-73, SEC-03
var (
	// ErrUnsafePath is returned when the named guest path, or something found
	// under it, could not be exported safely: an absolute or traversing path,
	// the private store, a symlinked source, an irregular file, or a
	// destination whose parent directory does not exist.
	ErrUnsafePath = errors.New("artifact export path is unsafe")

	// ErrSourceNotFound is returned when the named guest path does not exist
	// in the sandbox's staged worktree.
	ErrSourceNotFound = errors.New("artifact export source not found in the guest worktree")

	// ErrCollision is returned when the host destination (or its provenance
	// sidecar) already exists. Refusing is the collision policy: an export
	// never overwrites host state. Refs: ADR-011
	ErrCollision = errors.New("artifact export destination already exists")

	// ErrLimitExceeded is returned when the export would exceed its byte or
	// file-count ceiling (T7 resource abuse). Refs: MGIT-73
	ErrLimitExceeded = errors.New("artifact export exceeds its size or file-count limit")

	// ErrHardlinkEscape is returned when an exported file has links outside the
	// exported subtree: the artifact would alias a file the host never named.
	ErrHardlinkEscape = errors.New("artifact export file has links outside the exported subtree")
)

// Default limits bound one export. They are generous enough for a real
// node_modules tree (the artifact this exists for) and small enough that a
// runaway guest cannot fill a host disk before the ceiling stops it.
// Refs: MGIT-73 (T7)
const (
	// DefaultMaxBytes caps the total bytes one export may transfer.
	DefaultMaxBytes int64 = 4 << 30 // 4 GiB
	// DefaultMaxFiles caps the number of files and symlinks one export may
	// transfer.
	DefaultMaxFiles = 200_000
)

// Limits bound one export. A zero field selects the package default; a
// negative field is treated as zero (there is no "unlimited" — an unbounded
// guest->host transfer is the resource-abuse case this exists to prevent).
type Limits struct {
	MaxBytes int64 `json:"max_bytes"`
	MaxFiles int   `json:"max_files"`
}

// withDefaults fills unset ceilings.
func (l Limits) withDefaults() Limits {
	if l.MaxBytes <= 0 {
		l.MaxBytes = DefaultMaxBytes
	}
	if l.MaxFiles <= 0 {
		l.MaxFiles = DefaultMaxFiles
	}
	return l
}

// Provenance identifies the sandbox that produced an artifact. It is recorded
// in the sidecar manifest so a host cache never holds a tree of unknown
// origin. Refs: MGIT-61.15, ADR-011
type Provenance struct {
	SandboxID  string // host-owned sandbox ID
	TaskID     string // the task the sandbox is bound to
	Backend    string // sandbox backend that ran it (libkrun, vzf, ...)
	BaseDigest string // pinned guest base image digest, when the host has one
}

// Request is one host-initiated export. Every field is host-supplied: the
// guest contributes only the bytes under StagedTree.
type Request struct {
	// StagedTree is the host directory that IS the guest's worktree (the
	// backend resolves it; it is never guest-named).
	StagedTree string
	// GuestPath is the worktree-relative path to export.
	GuestPath string
	// HostPath is the absolute destination. It must not exist.
	HostPath string
	// Limits bound the transfer; zero fields take the package defaults.
	Limits Limits
	// Provenance is recorded in the sidecar manifest.
	Provenance Provenance
	// Now is the injected clock reading stamped on the manifest (no time.Now
	// in this package).
	Now time.Time
}

// Result reports what crossed the boundary, for the caller and for the
// append-only audit record.
type Result struct {
	HostPath     string `json:"host_path"`
	ManifestPath string `json:"manifest_path"`
	Files        int    `json:"files"`
	Bytes        int64  `json:"bytes"`
	// TreeHash is the SHA-256 over the canonical manifest of exported entries
	// (ADR-002: SHA-256 is mgit's authoritative hash). It identifies the
	// CONTENT, not the host location it landed at.
	TreeHash string `json:"tree_hash"`
}

// Export copies GuestPath out of a sandbox's staged worktree to HostPath.
//
// It validates everything first — the guest path, the whole subtree, and the
// destination — and only then writes, into a temporary directory beside the
// destination that is renamed into place. A refusal at any point leaves the
// host filesystem untouched, and a failure partway leaves nothing behind but
// the removed temporary. Refs: MGIT-73, SEC-03, ADR-011
func Export(req Request) (*Result, error) {
	p, err := buildPlan(req)
	if err != nil {
		return nil, err
	}
	dest, err := checkDestination(req.HostPath)
	if err != nil {
		return nil, err
	}
	return writeExport(req, p, dest)
}

// checkDestination validates the host-named destination and returns its
// cleaned form. The parent directory must already exist: an export creates the
// destination it was told to create and nothing else, so a typo cannot
// materialize a directory tree on the host. Both the destination and its
// sidecar must be absent — refusing is the collision policy. Refs: ADR-011
func checkDestination(hostPath string) (string, error) {
	if hostPath == "" {
		return "", fmt.Errorf("%w: the host destination must not be empty", ErrUnsafePath)
	}
	if !filepath.IsAbs(hostPath) {
		return "", fmt.Errorf("%w: the host destination %q must be an absolute path", ErrUnsafePath, hostPath)
	}
	dest := filepath.Clean(hostPath)
	parent := filepath.Dir(dest)
	info, err := os.Stat(parent)
	if err != nil || !info.IsDir() {
		return "", fmt.Errorf("%w: the host destination's parent directory %q does not exist", ErrUnsafePath, parent)
	}
	for _, path := range []string{dest, dest + ManifestSuffix} {
		if _, err := os.Lstat(path); err == nil {
			return "", fmt.Errorf("%w: %q (an export never overwrites host state; "+
				"choose another destination or remove it first)", ErrCollision, path)
		}
	}
	return dest, nil
}

// writeExport materializes a validated plan into a temporary directory beside
// the destination and renames it into place, so the artifact and its
// provenance sidecar appear together or not at all.
func writeExport(req Request, p *plan, dest string) (*Result, error) {
	tmp, err := os.MkdirTemp(filepath.Dir(dest), ".mgit-export-")
	if err != nil {
		return nil, fmt.Errorf("artifact export: create staging area: %w", err)
	}
	// The payload and the manifest are RENAMED out of tmp on success, so this
	// removes a leftover partial tree on failure and an empty directory on
	// success. Either way nothing of ours is left beside the destination.
	defer func() { _ = os.RemoveAll(tmp) }()

	payload := filepath.Join(tmp, "payload")
	entries, total, err := materialize(p, payload, req.Limits.withDefaults())
	if err != nil {
		return nil, err
	}
	manifest := newManifest(req, entries, total)
	tmpManifest := filepath.Join(tmp, "manifest.json")
	if err := writeManifestFile(tmpManifest, manifest); err != nil {
		return nil, err
	}
	if err := os.Rename(payload, dest); err != nil {
		return nil, fmt.Errorf("artifact export: place %s: %w", dest, err)
	}
	if err := os.Rename(tmpManifest, dest+ManifestSuffix); err != nil {
		// All-or-nothing: an artifact without its provenance record is exactly
		// the tree-of-unknown-origin this verb exists to prevent.
		_ = os.RemoveAll(dest)
		return nil, fmt.Errorf("artifact export: place the provenance manifest for %s: %w", dest, err)
	}
	return &Result{
		HostPath: dest, ManifestPath: dest + ManifestSuffix,
		Files: len(entries), Bytes: total, TreeHash: manifest.TreeHash,
	}, nil
}
