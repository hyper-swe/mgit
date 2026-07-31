// Package guestbase composes a libkrun guest base — a Linux userspace tree —
// from an OCI image, using only the standard library.
//
// WHY NO OCI CLIENT LIBRARY: the obvious choice, go-containerregistry's
// pkg/v1/remote, pulls sirupsen/logrus and docker/cli into the import graph
// (34 transitive packages, measured). logrus is an explicitly REJECTED
// package for core mgit (APPROVED-PACKAGES §4, "use log/slog") and is barred
// from non-sandbox trees by TestImports_SandboxDepsConfinedToSandboxd, so
// adding it here would both reintroduce a rejected dependency and break an
// existing guard. The subset of the registry API needed to pull a public
// image — a token, a manifest, some blobs — is small enough to own.
//
// WHAT THIS DELIBERATELY DOES NOT DO: no container runtime, no daemon, no
// image storage. Layers are streamed straight into a directory, because under
// libkrun the guest root IS a directory shared over virtio-fs.
//
// SECURITY POSTURE, stated because it reads backwards at first: letting a
// user load arbitrary tooling into the guest is safe BECAUSE the guest is the
// untrusted side. That is what the VM boundary is for, and a poisoned base
// burns a throwaway microVM. Everything that must stay protected — the host
// store, egress policy, the land airlock, attestation signing — is enforced
// HOST-side and is unaffected by what the tree contains. What this package
// still owes the user is INTEGRITY of what they asked for: every blob is
// verified against its digest as it is read.
// Refs: MGIT-61.15, ADR-010, FR-17.17
package guestbase

import (
	"encoding/hex"
	"fmt"
	"strings"
)

// Docker Hub's defaults, applied when a reference omits them. They are the
// reason "debian" and "registry-1.docker.io/library/debian:latest" name the
// same image.
const (
	defaultRegistry  = "registry-1.docker.io"
	defaultTag       = "latest"
	hubDefaultPrefix = "library/"
)

// Ref is a parsed OCI image reference.
//
// Tag and Digest are mutually exclusive as INPUT; after a pull, Digest holds
// the manifest digest the registry actually served, which is what goes into
// the provenance record. A tag is a moving pointer and is not, by itself,
// evidence of anything.
type Ref struct {
	Registry   string
	Repository string
	Tag        string // empty when the reference pinned a digest
	Digest     string // sha256:<hex>, set on input or filled in by the pull
}

// String renders the fully-resolved reference — registry, repository, and both
// the tag and the digest when both are known — never the user's shorthand. The
// audit trail must not inherit the ambiguity of defaults.
//
// Once a pull has resolved a tag, BOTH parts are kept: the digest is what makes
// the record repeatable, and the tag is the part a human recognizes.
func (r Ref) String() string {
	name := r.Registry + "/" + r.Repository
	if r.Tag != "" {
		name += ":" + r.Tag
	}
	if r.Digest != "" {
		name += "@" + r.Digest
	}
	return name
}

// ParseRef parses an image reference, applying Docker Hub's defaults.
//
// Distinguishing a registry host from the first path segment is the one
// subtle part: "myorg/tools" is a Hub repository, while "ghcr.io/acme/base"
// names a registry. The rule registries themselves use is that the first
// segment is a host only if it contains a dot or a colon, or is "localhost".
func ParseRef(in string) (Ref, error) {
	s := strings.TrimSpace(in)
	if s == "" {
		return Ref{}, fmt.Errorf("guest base: empty image reference")
	}

	ref := Ref{Registry: defaultRegistry}

	// Split off a digest or tag from the right, being careful that a
	// registry's port colon is not a tag separator.
	if name, digest, ok := strings.Cut(s, "@"); ok {
		if err := validateDigest(digest); err != nil {
			return Ref{}, err
		}
		ref.Digest = digest
		s = name
	}

	remainder := s
	if firstSegment, rest, ok := strings.Cut(s, "/"); ok && isRegistryHost(firstSegment) {
		ref.Registry = firstSegment
		remainder = rest
	}

	// Only now is a colon unambiguously a tag separator: any registry port
	// was consumed above.
	if repo, tag, ok := strings.Cut(remainder, ":"); ok {
		if tag == "" {
			return Ref{}, fmt.Errorf("guest base: %q has an empty tag", in)
		}
		remainder = repo
		if ref.Digest == "" {
			ref.Tag = tag
		}
	}
	if remainder == "" {
		return Ref{}, fmt.Errorf("guest base: %q names no repository", in)
	}

	// Hub's single-segment names live under library/.
	if ref.Registry == defaultRegistry && !strings.Contains(remainder, "/") {
		remainder = hubDefaultPrefix + remainder
	}
	ref.Repository = remainder

	if ref.Tag == "" && ref.Digest == "" {
		ref.Tag = defaultTag
	}
	return ref, nil
}

// isRegistryHost reports whether a leading path segment names a registry
// rather than a repository namespace.
func isRegistryHost(seg string) bool {
	return strings.ContainsAny(seg, ".:") || seg == "localhost"
}

// validateDigest checks the sha256:<64 hex> form. A malformed digest must be
// rejected here rather than becoming a confusing 404 from the registry.
func validateDigest(d string) error {
	algo, hexPart, ok := strings.Cut(d, ":")
	if !ok || algo != "sha256" {
		return fmt.Errorf("guest base: digest %q must be sha256:<hex>", d)
	}
	if len(hexPart) != 64 {
		return fmt.Errorf("guest base: digest %q must have 64 hex characters, got %d", d, len(hexPart))
	}
	if _, err := hex.DecodeString(hexPart); err != nil {
		return fmt.Errorf("guest base: digest %q is not hexadecimal: %w", d, err)
	}
	return nil
}
