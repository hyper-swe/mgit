package guestbase

import (
	_ "embed"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
)

// releaseBaseJSON is the guest base image this release is smoke-tested with,
// embedded so the binary can say which base it vouches for. The file is
// committed and refreshed by scripts/release/pin-guest-base.sh before a cut;
// the release smoke composes FROM it, so what was tested is what is recorded.
//
//go:embed release-base.json
var releaseBaseJSON []byte

// ErrNoReleaseBase means this build carries no release base record — the
// comparison doctor makes against it cannot be made, and says so.
var ErrNoReleaseBase = errors.New("guest base: this build records no release guest base")

// ReleaseBase is the image and digest a release was smoke-tested with.
//
// The image is a NAME (registry/repo:tag, or its Hub shorthand): documentation
// for a human. The digest is the identity: what `sandbox base from` with no
// reference composes, and what doctor compares a repository's composed base
// against. A tag moves; a digest cannot. Refs: MGIT-219, MGIT-218, MGIT-147
type ReleaseBase struct {
	Image  string `json:"image"`
	Digest string `json:"digest"`
}

// ReleaseBaseRecord returns the base this build vouches for, or
// ErrNoReleaseBase when the embedded record is empty.
func ReleaseBaseRecord() (ReleaseBase, error) {
	return parseReleaseBase(releaseBaseJSON)
}

// parseReleaseBase reads a record and refuses one that could not pin
// anything: an empty file, a malformed digest, or an image that already
// carries a digest (the record's two halves must stay distinct).
func parseReleaseBase(raw []byte) (ReleaseBase, error) {
	if strings.TrimSpace(string(raw)) == "" {
		return ReleaseBase{}, ErrNoReleaseBase
	}
	var rec ReleaseBase
	if err := json.Unmarshal(raw, &rec); err != nil {
		return ReleaseBase{}, fmt.Errorf("guest base: release record: %w", err)
	}
	if rec.Image == "" || rec.Digest == "" {
		return ReleaseBase{}, fmt.Errorf("guest base: release record names image %q and digest %q; both are required", rec.Image, rec.Digest)
	}
	if err := validateDigest(rec.Digest); err != nil {
		return ReleaseBase{}, fmt.Errorf("guest base: release record: %w", err)
	}
	ref, err := ParseRef(rec.Image)
	if err != nil {
		return ReleaseBase{}, fmt.Errorf("guest base: release record: %w", err)
	}
	if ref.Digest != "" {
		return ReleaseBase{}, fmt.Errorf("guest base: release record: the image %q carries a digest; the digest field is where it goes", rec.Image)
	}
	return rec, nil
}

// Ref renders the record as the fully-resolved reference `sandbox base from`
// composes and provenance records: registry/repo:tag@sha256:…, both halves
// kept — the tag for a reader, the digest for the pull.
func (r ReleaseBase) Ref() string {
	ref, err := ParseRef(r.Image)
	if err != nil {
		return r.Image + "@" + r.Digest
	}
	ref.Digest = r.Digest
	return ref.String()
}
