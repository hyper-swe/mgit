package packaging

import (
	"go/version"
	"regexp"
	"testing"
)

// Both build paths must use a compiler with the GO-2026-6809 fix. The affected
// ranges come from the published Go advisory, not from the current pin. Refs: MGIT-314
func TestGoToolchain_GO20266809Patched(t *testing.T) {
	for _, tc := range []struct{ name, path, pattern string }{
		{"module", "go.mod", `(?m)^go (\S+)$`},
		{"guest", "scripts/sandbox-image/guest-tools.env", `(?m)^GO_VERSION=(\S+)$`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			match := regexp.MustCompile(tc.pattern).FindStringSubmatch(readRepoFile(t, tc.path))
			if len(match) != 2 {
				t.Fatalf("%s must declare an exact Go version", tc.path)
			}
			v := "go" + match[1]
			if !version.IsValid(v) || version.Compare(v, "go1.26.11") < 0 ||
				(version.Compare(v, "go1.27") >= 0 && version.Compare(v, "go1.27.3") < 0) {
				t.Fatalf("%s pins %s, affected by GO-2026-6809; use a patched compiler", tc.path, v)
			}
		})
	}
}

// Expected sums are the Go publisher's go1.26.11 Linux archive values, captured
// independently of the manifest. Refs: MGIT-314
func TestGuestGoArchives_PublisherChecksums(t *testing.T) {
	manifest := readRepoFile(t, "scripts/sandbox-image/guest-tools.env")
	for _, tc := range []struct{ key, want string }{
		{"GO_SHA256_AMD64", "9849f4e04ee35303714e34fbc3ac8ab44ea3e448a95aa182f91e3b9b79c710ec"},
		{"GO_SHA256_ARM64", "6c2f29b458b5add0835f18b776f69c8881a6d88428f1484a6da55755e620757e"},
	} {
		t.Run(tc.key, func(t *testing.T) {
			match := regexp.MustCompile(`(?m)^` + tc.key + `=(\S+)$`).FindStringSubmatch(manifest)
			if len(match) != 2 || match[1] != tc.want {
				t.Fatalf("%s must match the Go 1.26.11 publisher checksum", tc.key)
			}
		})
	}
}
