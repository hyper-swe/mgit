package reviewbind

import (
	"encoding/json"
	"errors"
	"github.com/hyper-swe/mgit/internal/reviewprep"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func recordJSON(t *testing.T, id string) []byte {
	t.Helper()
	b, err := json.Marshal(goodMap(id))
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// okResolver stands for the backend image store: "reviewer" is registered with the fixture's digest.
func okResolver(_ string, name string) (string, error) {
	if name == "reviewer" {
		return "sha256:" + strings.Repeat("b", 64), nil
	}
	return "", errors.New("no such image")
}

func newRoot(t *testing.T) string { return filepath.Join(t.TempDir(), "host") }

func TestInstall_WritesAPinnedMapTheLoaderAccepts(t *testing.T) {
	root := newRoot(t)
	info, err := Install(root, recordJSON(t, "m1"), false, okResolver)
	if err != nil {
		t.Fatal(err)
	}
	m, err := LoadMap(root, "m1")
	if err != nil || m.ID != "m1" {
		t.Fatalf("loader refused what install wrote: %v", err)
	}
	h, _ := CanonicalHash(*m)
	if info.ID != "m1" || info.MapSHA256 != h || info.BindingSHA256 != m.BindingSHA256 || info.MapSHA256 == info.BindingSHA256 {
		t.Fatalf("info does not carry the two distinct hashes: %+v", info)
	}
	if info.ImageDigest != m.ImageDigest || !strings.HasPrefix(info.ImageDigest, "sha256:") {
		t.Fatalf("info lacks the record's image digest: %+v", info)
	}
	for _, d := range []string{root, filepath.Join(root, "review"), filepath.Join(root, "review", "maps")} {
		fi, err := os.Stat(d)
		if err != nil || fi.Mode().Perm() != 0o700 {
			t.Errorf("%s mode %v err %v, want 0700", d, fi.Mode().Perm(), err)
		}
	}
	for _, f := range []string{"m1.json", "m1.sha256"} {
		fi, err := os.Stat(filepath.Join(root, "review", "maps", f))
		if err != nil || fi.Mode().Perm() != 0o600 {
			t.Errorf("%s mode %v err %v, want 0600", f, fi.Mode().Perm(), err)
		}
	}
}

func TestInstall_RefusesWhatTheLoaderWouldRefuse(t *testing.T) {
	bad := goodMap("m2")
	bad.EgressPolicyID = "host:127.0.0.1"
	raw, _ := json.Marshal(bad)
	for name, in := range map[string][]byte{
		"bad egress": raw, "not json": []byte("{"), "trailing": append(recordJSON(t, "m3"), []byte(` {}`)...),
		"unknown key": []byte(strings.Replace(string(recordJSON(t, "m4")), `"id"`, `"extra":1,"id"`, 1)),
		"bad id":      recordJSON(t, "../x"),
	} {
		root := newRoot(t)
		if _, err := Install(root, in, false, okResolver); err == nil {
			t.Errorf("%s was installed", name)
		}
	}
}

func TestInstall_NeverOverwritesAnInstalledID(t *testing.T) {
	root := newRoot(t)
	if _, err := Install(root, recordJSON(t, "m5"), false, okResolver); err != nil {
		t.Fatal(err)
	}
	other := goodMap("m5")
	other.Argv = []string{"--other"}
	raw, _ := json.Marshal(other)
	if _, err := Install(root, raw, false, okResolver); err == nil {
		t.Fatal("an installed id was overwritten")
	}
	m, err := LoadMap(root, "m5")
	if err != nil || len(m.Argv) != 1 || m.Argv[0] != "--json" {
		t.Fatalf("the original changed: %v %+v", err, m)
	}
}

func TestInstall_DefaultIsPinnedAndShowResolvesIt(t *testing.T) {
	root := newRoot(t)
	if _, err := Install(root, recordJSON(t, "m6"), true, okResolver); err != nil {
		t.Fatal(err)
	}
	if id, err := DefaultID(root); err != nil || id != "m6" {
		t.Fatalf("default %q %v", id, err)
	}
	info, err := Show(root, "")
	if err != nil || info.ID != "m6" {
		t.Fatalf("show of the default: %+v %v", info, err)
	}
	if err := os.WriteFile(filepath.Join(root, "review", "maps", "_default"), []byte("other\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := DefaultID(root); err == nil {
		t.Fatal("an edited default pointer was accepted")
	}
}

func TestShow_NoDefaultSaysWhatToRun(t *testing.T) {
	root := newRoot(t)
	if _, err := Install(root, recordJSON(t, "m7"), false, okResolver); err != nil {
		t.Fatal(err)
	}
	_, err := Show(root, "")
	if err == nil || !strings.Contains(err.Error(), "mgit sandbox review map install --file <map.json> --default") {
		t.Fatalf("a missing default must say how to install one: %v", err)
	}
}

func TestShow_NeverCarriesACredentialField(t *testing.T) {
	root := newRoot(t)
	m := goodMap("m8")
	m.CredentialRef, m.EgressPolicyID = "review-key", "host:api.example.com"
	raw, _ := json.Marshal(m)
	if _, err := Install(root, raw, false, okResolver); err != nil {
		t.Fatal(err)
	}
	info, err := Show(root, "m8")
	if err != nil {
		t.Fatal(err)
	}
	j, _ := json.Marshal(info)
	if strings.Contains(string(j), "review-key") || strings.Contains(strings.ToLower(string(j)), "credential") {
		t.Fatalf("show output names the credential: %s", j)
	}
	if !strings.Contains(string(j), `"schema":"mgit.review-map-info/v1"`) {
		t.Fatalf("show output is not versioned: %s", j)
	}
}

func TestInstall_RefusesAnUnsafeExistingDirectory(t *testing.T) {
	root := newRoot(t)
	if err := os.MkdirAll(filepath.Join(root, "review", "maps"), 0o755); err != nil { //nolint:gosec // unsafe on purpose
		t.Fatal(err)
	}
	if err := os.Chmod(filepath.Join(root, "review", "maps"), 0o777); err != nil { //nolint:gosec // unsafe on purpose
		t.Fatal(err)
	}
	if _, err := Install(root, recordJSON(t, "m9"), false, okResolver); err == nil {
		t.Fatal("installed into a world-writable maps directory")
	}
}

func TestInstall_RefusesAnImageRefTheStoreDoesNotResolve(t *testing.T) {
	m := goodMap("r1")
	m.ImageRef = "unregistered@" + m.ImageDigest
	raw, _ := json.Marshal(m)
	root := newRoot(t)
	if _, err := Install(root, raw, false, okResolver); err == nil {
		t.Fatal("an image name the store does not know was installed")
	}
	if _, err := os.Stat(filepath.Join(root, "review", "maps", "r1.json")); err == nil {
		t.Fatal("a refused map left files behind")
	}
}

func TestInstall_RefusesAnImageRefThatDisagreesWithItsDigest(t *testing.T) {
	for name, edit := range map[string]func(*reviewprep.Mapping){
		"ref digest differs from image_digest": func(m *reviewprep.Mapping) { m.ImageRef = "reviewer@sha256:" + strings.Repeat("c", 64) },
		"ref has no digest":                    func(m *reviewprep.Mapping) { m.ImageRef = "reviewer:latest" },
		"no image_ref at all":                  func(m *reviewprep.Mapping) { m.ImageRef = "" },
	} {
		m := goodMap("r2")
		edit(&m)
		raw, _ := json.Marshal(m)
		if _, err := Install(newRoot(t), raw, false, okResolver); err == nil {
			t.Errorf("%s was installed", name)
		}
	}
	bad := func(string, string) (string, error) { return "sha256:" + strings.Repeat("d", 64), nil }
	if _, err := Install(newRoot(t), recordJSON(t, "r3"), false, bad); err == nil {
		t.Fatal("a store entry with another digest was accepted")
	}
	if _, err := Install(newRoot(t), recordJSON(t, "r4"), false, nil); err == nil {
		t.Fatal("install ran with no image store to check against")
	}
}

func TestShow_CarriesTheExactImageRef(t *testing.T) {
	root := newRoot(t)
	if _, err := Install(root, recordJSON(t, "r5"), false, okResolver); err != nil {
		t.Fatal(err)
	}
	info, err := Show(root, "r5")
	if err != nil || info.ImageRef != "reviewer@sha256:"+strings.Repeat("b", 64) {
		t.Fatalf("%+v %v", info, err)
	}
	j, _ := json.Marshal(info)
	if !strings.Contains(string(j), `"image_ref":"reviewer@sha256:`) {
		t.Fatalf("json name: %s", j)
	}
}
