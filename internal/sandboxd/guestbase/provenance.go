package guestbase

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// ProvenanceFileName is the append-only journal of every guest base this
// repository has composed, one JSON object per line, under the host config
// root. Refs: MGIT-147
const ProvenanceFileName = "base-provenance.jsonl"

// Compose is one composition of a guest base: what was asked for, what that
// resolved to, and what came out.
//
// WHY THIS FILE EXISTS. images.lock holds ONE entry per name, so the moment a
// base is recomposed the previous pin is gone from it — and with it any way to
// answer the question a user actually asks, which is not "what is pinned" but
// "why did what I pinned change". That question came from the field: the guest
// base digest moved under the SAME TAG (golang:1.26-bookworm, 5290458b ->
// 8ed5e7e2) between two days, and nobody could tell whether upstream had moved
// the tag or our composition had changed, because a tag was being used as the
// identity. A tag is a name that can point twice; a digest cannot.
//
// So a compose records BOTH: the tag as PROVENANCE (the part a human
// recognizes and retypes) and the digest as IDENTITY (the part that names
// exactly one byte sequence), plus what it superseded. Append-only, like every
// other audit surface in mgit — a superseding entry never edits the one before
// it. Refs: MGIT-147, MGIT-105, FR-17.17
type Compose struct {
	RecordedAt string `json:"recorded_at"` // RFC3339 UTC
	Name       string `json:"name"`        // images.lock name, e.g. "base"
	SourceTag  string `json:"source_tag"`  // provenance: registry/repo:tag, as resolved from what the user typed
	SourceRef  string `json:"source_ref"`  // identity of the INPUT: registry/repo:tag@sha256:...
	BaseDigest string `json:"base_digest"` // identity of the OUTPUT: the composed tree's digest
	// What this compose replaced in images.lock, when it replaced anything.
	// Empty on a first compose.
	PrevSourceRef  string `json:"prev_source_ref,omitempty"`
	PrevBaseDigest string `json:"prev_base_digest,omitempty"`
}

// SourceTag splits the human half out of a resolved reference:
// "registry/repo:tag" from "registry/repo:tag@sha256:...". It is what a
// remedy message should tell a user to retype — never the digest, which they
// never chose. Refs: MGIT-147
func SourceTag(sourceRef string) string {
	tag, _, _ := strings.Cut(sourceRef, "@")
	return tag
}

// SourceDigest splits the identity half out of a resolved reference.
func SourceDigest(sourceRef string) string {
	_, digest, _ := strings.Cut(sourceRef, "@")
	return digest
}

// TagMoved reports that the SAME tag now resolves to a DIFFERENT image than
// the one previously pinned.
//
// This is the case worth shouting about, and it is distinct from both "you
// composed a different image" (the tags differ — you asked for that) and "you
// recomposed the same image" (the digests agree — nothing moved). Refs: MGIT-147
func (c Compose) TagMoved() bool {
	if c.PrevSourceRef == "" || c.SourceRef == "" {
		return false
	}
	if SourceTag(c.PrevSourceRef) != SourceTag(c.SourceRef) {
		return false
	}
	return SourceDigest(c.PrevSourceRef) != SourceDigest(c.SourceRef)
}

// RecordCompose appends one composition to the journal under hostRoot.
//
// The write is O_APPEND on a line-delimited file, which is what makes it
// append-only in practice as well as in policy: nothing rewrites an earlier
// line, and a concurrent compose in another worktree appends its own.
// Refs: MGIT-147
func RecordCompose(hostRoot string, rec Compose, clock func() time.Time) error {
	if hostRoot == "" {
		return fmt.Errorf("guest base provenance: host root must not be empty")
	}
	if clock == nil {
		return fmt.Errorf("guest base provenance: clock must not be nil")
	}
	rec.RecordedAt = clock().UTC().Format(time.RFC3339)
	line, err := json.Marshal(rec)
	if err != nil {
		return fmt.Errorf("guest base provenance: encode record: %w", err)
	}
	if err := os.MkdirAll(hostRoot, 0o750); err != nil {
		return fmt.Errorf("guest base provenance: create %s: %w", hostRoot, err)
	}
	path := filepath.Join(hostRoot, ProvenanceFileName)
	//nolint:gosec // host-owned config path; 0600 below keeps it owner-only
	file, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return fmt.Errorf("guest base provenance: open %s: %w", path, err)
	}
	if _, err := file.Write(append(line, '\n')); err != nil {
		_ = file.Close()
		return fmt.Errorf("guest base provenance: append to %s: %w", path, err)
	}
	if err := file.Close(); err != nil {
		return fmt.Errorf("guest base provenance: close %s: %w", path, err)
	}
	return nil
}

// ComposeHistory reads the journal oldest-first. A repo that has never
// composed a base has no journal and no history, which is not an error.
func ComposeHistory(hostRoot string) ([]Compose, error) {
	path := filepath.Join(hostRoot, ProvenanceFileName)
	file, err := os.Open(path) //nolint:gosec // host-owned config path
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("guest base provenance: open %s: %w", path, err)
	}
	defer func() { _ = file.Close() }()

	var history []Compose
	scanner := bufio.NewScanner(file)
	// A composed reference is short; the default 64 KiB line budget is ample,
	// but a corrupt journal must not be able to allocate without bound.
	scanner.Buffer(make([]byte, 0, 4096), 1<<20)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" {
			continue
		}
		var rec Compose
		if err := json.Unmarshal([]byte(line), &rec); err != nil {
			return nil, fmt.Errorf("guest base provenance: parse %s: %w", path, err)
		}
		history = append(history, rec)
	}
	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("guest base provenance: read %s: %w", path, err)
	}
	return history, nil
}
