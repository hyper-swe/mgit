package artifactexport

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// ManifestSuffix is appended to the destination path to name the provenance
// sidecar. It is a SIDECAR rather than a file inside the artifact so the
// exported tree is byte-for-byte what the guest built — dropping a manifest
// into a node_modules tree would change the artifact it describes.
const ManifestSuffix = ".mgit-export.json"

// ManifestSchema identifies the sidecar format. It is versioned because a
// consumer (a provisioning cache) must be able to tell an older record from a
// newer one rather than guessing from which fields are populated — the same
// reasoning as the attestation payload version. Refs: MGIT-61.15
const ManifestSchema = "mgit.artifact-export/v1"

// ManifestEntry is one exported file or symlink. Regular files carry their
// SHA-256 (ADR-002: mgit's authoritative hash); symlinks carry their target
// text and are never followed. Directories are implied by their entries.
type ManifestEntry struct {
	Path    string `json:"path"`              // path relative to the exported root
	Mode    string `json:"mode,omitempty"`    // octal permission bits, e.g. "0644"
	Size    int64  `json:"size,omitempty"`    // bytes actually copied
	SHA256  string `json:"sha256,omitempty"`  // hex SHA-256 of the copied bytes
	Symlink string `json:"symlink,omitempty"` // link target text (symlinks only)
}

// Manifest is the provenance sidecar written beside an exported artifact: it
// answers "which sandbox produced this, from which task, on which base image,
// and exactly what is in it".
//
// A node_modules tree sitting in a host cache with no such record is a
// supply-chain artifact of unknown origin; this is the file-level counterpart
// of the commit attestation the land path issues. Refs: MGIT-73, MGIT-61.15, ADR-011
type Manifest struct {
	Schema     string          `json:"schema"`
	SandboxID  string          `json:"sandbox_id"`
	TaskID     string          `json:"task_id"`
	Backend    string          `json:"backend,omitempty"`
	BaseDigest string          `json:"base_digest,omitempty"`
	GuestPath  string          `json:"guest_path"`
	HostPath   string          `json:"host_path"`
	ExportedAt time.Time       `json:"exported_at"`
	FileCount  int             `json:"file_count"`
	ByteCount  int64           `json:"byte_count"`
	TreeHash   string          `json:"tree_hash"`
	Entries    []ManifestEntry `json:"entries"`
}

// newManifest assembles the sidecar for a completed materialization.
func newManifest(req Request, entries []ManifestEntry, total int64) Manifest {
	sorted := append([]ManifestEntry(nil), entries...)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i].Path < sorted[j].Path })
	return Manifest{
		Schema:     ManifestSchema,
		SandboxID:  req.Provenance.SandboxID,
		TaskID:     req.Provenance.TaskID,
		Backend:    req.Provenance.Backend,
		BaseDigest: req.Provenance.BaseDigest,
		GuestPath:  req.GuestPath,
		HostPath:   filepath.Clean(req.HostPath),
		ExportedAt: req.Now.UTC(),
		FileCount:  len(sorted),
		ByteCount:  total,
		TreeHash:   treeHash(sorted),
		Entries:    sorted,
	}
}

// treeHash is the SHA-256 over a canonical rendering of the exported entries
// (path, kind, mode, size, content hash), sorted by path. It identifies the
// CONTENT of an artifact independently of where it landed, so a cache can tell
// two exports of the same tree apart from two different trees. Refs: ADR-002
func treeHash(entries []ManifestEntry) string {
	h := sha256.New()
	var line strings.Builder
	for _, e := range entries {
		line.Reset()
		if e.Symlink != "" {
			fmt.Fprintf(&line, "l\x00%s\x00%s\n", e.Path, e.Symlink)
		} else {
			fmt.Fprintf(&line, "f\x00%s\x00%s\x00%d\x00%s\n", e.Path, e.Mode, e.Size, e.SHA256)
		}
		h.Write([]byte(line.String()))
	}
	return hex.EncodeToString(h.Sum(nil))
}

// writeManifestFile writes the sidecar into the export's temporary directory,
// from where it is renamed beside the artifact.
func writeManifestFile(path string, m Manifest) error {
	data, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return fmt.Errorf("artifact export: encode the provenance manifest: %w", err)
	}
	data = append(data, '\n')
	if err := os.WriteFile(path, data, 0o600); err != nil {
		return fmt.Errorf("artifact export: write the provenance manifest: %w", err)
	}
	return nil
}
