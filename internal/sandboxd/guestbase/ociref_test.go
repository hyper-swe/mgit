package guestbase

import (
	"strings"
	"testing"
)

// Reference parsing is the first thing a user's typo hits, so its errors must
// be about THEIR input, not our internals. Refs: MGIT-61.15
func TestParseRef(t *testing.T) {
	tests := []struct {
		name                string
		in                  string
		registry, repo, tag string
		digest              string
		wantErr             string
	}{
		{
			// Docker Hub shorthand: no registry, no namespace, no tag.
			name: "bare_name_gets_hub_defaults",
			in:   "debian", registry: "registry-1.docker.io", repo: "library/debian", tag: "latest",
		},
		{
			name: "hub_with_namespace_and_tag",
			in:   "myorg/tools:1.2", registry: "registry-1.docker.io", repo: "myorg/tools", tag: "1.2",
		},
		{
			name: "explicit_registry",
			in:   "ghcr.io/acme/base:v3", registry: "ghcr.io", repo: "acme/base", tag: "v3",
		},
		{
			name: "registry_with_port",
			in:   "localhost:5000/base:dev", registry: "localhost:5000", repo: "base", tag: "dev",
		},
		{
			// A digest reference is the strongest form: no tag resolution.
			name:     "by_digest",
			in:       "debian@sha256:" + strings.Repeat("a", 64),
			registry: "registry-1.docker.io", repo: "library/debian",
			digest: "sha256:" + strings.Repeat("a", 64),
		},
		{name: "empty", in: "", wantErr: "empty"},
		{name: "malformed_digest", in: "debian@sha256:xyz", wantErr: "digest"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := ParseRef(tt.in)
			if tt.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
					t.Fatalf("ParseRef(%q) = %v, want error mentioning %q", tt.in, err, tt.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("ParseRef(%q): %v", tt.in, err)
			}
			if got.Registry != tt.registry || got.Repository != tt.repo {
				t.Errorf("got registry=%q repo=%q, want %q / %q",
					got.Registry, got.Repository, tt.registry, tt.repo)
			}
			if got.Tag != tt.tag || got.Digest != tt.digest {
				t.Errorf("got tag=%q digest=%q, want %q / %q", got.Tag, got.Digest, tt.tag, tt.digest)
			}
		})
	}
}

func TestRef_StringRoundTripsWhatWasResolved(t *testing.T) {
	// The recorded provenance must name the registry and repo explicitly,
	// never the user's shorthand — "debian" means different things depending
	// on defaults, and the audit trail must not inherit that ambiguity.
	r, err := ParseRef("debian:12")
	if err != nil {
		t.Fatal(err)
	}
	got := r.String()
	for _, want := range []string{"registry-1.docker.io", "library/debian", "12"} {
		if !strings.Contains(got, want) {
			t.Errorf("String() = %q, does not name %q", got, want)
		}
	}
}
