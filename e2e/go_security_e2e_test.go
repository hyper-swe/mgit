package e2e

import (
	"debug/buildinfo"
	"go/version"
	"strings"
	"testing"
)

// Inspect the actual CLI artifact, not just the source pin. Refs: MGIT-314
// CI selects go.mod with GOTOOLCHAIN=local. A newer local compiler can pass
// this artifact check despite an old pin; the packaging test checks that pin.
func TestE2E_GoToolchain_PatchedCompilerMetadata(t *testing.T) {
	bin := buildMgitBinary(t)
	info, err := buildinfo.ReadFile(bin)
	if err != nil {
		t.Fatal(err)
	}
	v := info.GoVersion
	if !version.IsValid(v) || version.Compare(v, "go1.26.10") < 0 ||
		(version.Compare(v, "go1.27") >= 0 && version.Compare(v, "go1.27.3") < 0) {
		t.Fatalf("CLI compiler %s is affected by GO-2026-6809", v)
	}
	out := mustMgit(t, bin, t.TempDir(), "version")
	if strings.TrimSpace(out) == "" {
		t.Fatal("mgit version returned no build metadata")
	}
	t.Logf("CLI built with %s; version: %s", v, strings.TrimSpace(out))
}
