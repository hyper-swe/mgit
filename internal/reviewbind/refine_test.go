package reviewbind

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// pinned writes raw as the record and pins its canonical hash, the way an
// operator would, but with bytes the operator did not produce through install.
func pinned(t *testing.T, root, id string, raw []byte) {
	t.Helper()
	ownerOnly(t, root)
	dir := filepath.Join(root, "review", "maps")
	if err := os.MkdirAll(dir, 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, id+".json"), raw, 0o600); err != nil {
		t.Fatal(err)
	}
	h, err := CanonicalHashJSON(raw)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, id+".sha256"), []byte(h+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestLoadMap_CaseVariantKeyIsRefused(t *testing.T) {
	root := t.TempDir()
	raw, _ := json.Marshal(goodMap("m4"))
	raw = append(append([]byte{}, raw[:len(raw)-1]...), []byte(`,"EGRESS_POLICY_ID":"host:evil.example.com"}`)...)
	pinned(t, root, "m4", raw)
	if m, err := LoadMap(root, "m4"); err == nil {
		t.Fatalf("a case-variant key was accepted, egress=%q", m.EgressPolicyID)
	}
}

func TestLoadMap_BytesAfterThePinnedRecordAreRefused(t *testing.T) {
	root := t.TempDir()
	install(t, root, goodMap("m3"))
	p := filepath.Join(root, "review", "maps", "m3.json")
	f, err := os.OpenFile(p, os.O_APPEND|os.O_WRONLY, 0) //nolint:gosec // p is the test's own temp file
	if err != nil {
		t.Fatal(err)
	}
	_, _ = f.WriteString(` {"egress_policy_id":"host:evil.example.com"}`)
	_ = f.Close()
	if _, err := LoadMap(root, "m3"); err == nil {
		t.Fatal("bytes appended after the pin still loaded")
	}
}

func TestHashJSON_TrailingBytesAreAnError(t *testing.T) {
	if _, err := CanonicalHashJSON([]byte(`{"a":1} {"b":2}`)); err == nil {
		t.Fatal("trailing value hashed")
	}
}

func TestLoadMap_MapsDirectoryMustBeOwnerOnly(t *testing.T) {
	root := t.TempDir()
	install(t, root, goodMap("m2"))
	if err := os.Chmod(filepath.Join(root, "review", "maps"), 0o777); err != nil { //nolint:gosec // the test makes it unsafe on purpose
		t.Fatal(err)
	}
	if _, err := LoadMap(root, "m2"); err == nil {
		t.Fatal("a world-writable maps directory was accepted")
	}
}

func TestLoadMap_MapsDirectoryMustNotBeALink(t *testing.T) {
	root, elsewhere := t.TempDir(), t.TempDir()
	ownerOnly(t, root)
	install(t, elsewhere, goodMap("m1"))
	if err := os.MkdirAll(filepath.Join(root, "review"), 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(elsewhere, "review", "maps"), filepath.Join(root, "review", "maps")); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadMap(root, "m1"); err == nil {
		t.Fatal("a linked maps directory was accepted")
	}
}

func TestLoadMap_ReviewDirectoryMustNotBeALink(t *testing.T) {
	root, elsewhere := t.TempDir(), t.TempDir()
	ownerOnly(t, root)
	install(t, elsewhere, goodMap("m1"))
	if err := os.Symlink(filepath.Join(elsewhere, "review"), filepath.Join(root, "review")); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadMap(root, "m1"); err == nil {
		t.Fatal("a linked review directory was accepted")
	}
}

func TestLoadMap_OversizeRecordIsRefused(t *testing.T) {
	root := t.TempDir()
	m := goodMap("big")
	m.Argv = []string{strings.Repeat("x", maxMapBytes+1)}
	install(t, root, m)
	if _, err := LoadMap(root, "big"); err == nil {
		t.Fatal("a record over the size cap loaded")
	}
}

func TestParseEgress_AddressSpellingsAreRefused(t *testing.T) {
	for _, h := range []string{"host:0x7f.0.0.1", "host:0x7f.1", "host:a.123", "host:a.0x7f", "host:0177.0.0.01", "host:127.0.0.1", "host:a.0"} {
		if p, err := ParseEgress(h); err == nil {
			t.Errorf("%s accepted as %q", h, p.Host)
		}
	}
	if _, err := ParseEgress("host:a1.example.com"); err != nil {
		t.Errorf("a plain name refused: %v", err)
	}
}

func TestLoadMap_HostRootMustBeOwnerOnly(t *testing.T) {
	root := t.TempDir()
	install(t, root, goodMap("m5"))
	if err := os.Chmod(root, 0o777); err != nil { //nolint:gosec // the test makes it unsafe on purpose
		t.Fatal(err)
	}
	if _, err := LoadMap(root, "m5"); err == nil {
		t.Fatal("a world-writable host root was accepted")
	}
}
