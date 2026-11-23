package reviewbind

import (
	"encoding/json"
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

func newRoot(t *testing.T) string { return filepath.Join(t.TempDir(), "host") }

func TestInstall_WritesAPinnedMapTheLoaderAccepts(t *testing.T) {
	root := newRoot(t)
	info, err := Install(root, recordJSON(t, "m1"), false)
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
		if _, err := Install(root, in, false); err == nil {
			t.Errorf("%s was installed", name)
		}
	}
}

func TestInstall_NeverOverwritesAnInstalledID(t *testing.T) {
	root := newRoot(t)
	if _, err := Install(root, recordJSON(t, "m5"), false); err != nil {
		t.Fatal(err)
	}
	other := goodMap("m5")
	other.Argv = []string{"--other"}
	raw, _ := json.Marshal(other)
	if _, err := Install(root, raw, false); err == nil {
		t.Fatal("an installed id was overwritten")
	}
	m, err := LoadMap(root, "m5")
	if err != nil || len(m.Argv) != 1 || m.Argv[0] != "--json" {
		t.Fatalf("the original changed: %v %+v", err, m)
	}
}

func TestInstall_DefaultIsPinnedAndShowResolvesIt(t *testing.T) {
	root := newRoot(t)
	if _, err := Install(root, recordJSON(t, "m6"), true); err != nil {
		t.Fatal(err)
	}
	if id, err := DefaultID(root); err != nil || id != "m6" {
		t.Fatalf("default %q %v", id, err)
	}
	info, err := Show(root, "", namesOf("reviewer"))
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
	if _, err := Install(root, recordJSON(t, "m7"), false); err != nil {
		t.Fatal(err)
	}
	_, err := Show(root, "", namesOf("reviewer"))
	if err == nil || !strings.Contains(err.Error(), "mgit sandbox review map install --file <map.json> --default") {
		t.Fatalf("a missing default must say how to install one: %v", err)
	}
}

func TestShow_NeverCarriesACredentialField(t *testing.T) {
	root := newRoot(t)
	m := goodMap("m8")
	m.CredentialRef, m.EgressPolicyID = "review-key", "host:api.example.com"
	raw, _ := json.Marshal(m)
	if _, err := Install(root, raw, false); err != nil {
		t.Fatal(err)
	}
	info, err := Show(root, "m8", namesOf("reviewer"))
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
	if _, err := Install(root, recordJSON(t, "m9"), false); err == nil {
		t.Fatal("installed into a world-writable maps directory")
	}
}

func namesOf(names ...string) ImageNames {
	return func(string, string) ([]string, error) { return names, nil }
}

func TestShow_ResolvesTheDigestToTheOneRegisteredImage(t *testing.T) {
	root := newRoot(t)
	if _, err := Install(root, recordJSON(t, "r5"), false); err != nil {
		t.Fatal(err)
	}
	info, err := Show(root, "r5", namesOf("reviewer"))
	if err != nil || info.ImageRef != "reviewer@sha256:"+strings.Repeat("b", 64) {
		t.Fatalf("%+v %v", info, err)
	}
	j, _ := json.Marshal(info)
	if !strings.Contains(string(j), `"image_ref":"reviewer@sha256:`) || !strings.Contains(string(j), `"image_digest":"sha256:`) {
		t.Fatalf("json names: %s", j)
	}
}

func TestShow_NoRegisteredImageForTheDigestIsRefused(t *testing.T) {
	root := newRoot(t)
	if _, err := Install(root, recordJSON(t, "r6"), false); err != nil {
		t.Fatal(err)
	}
	if _, err := Show(root, "r6", namesOf()); err == nil || !strings.Contains(err.Error(), "register it in the image store") {
		t.Fatalf("zero matches must refuse and say what to do: %v", err)
	}
}

func TestShow_TwoRegisteredImagesForTheDigestAreRefused(t *testing.T) {
	root := newRoot(t)
	if _, err := Install(root, recordJSON(t, "r7"), false); err != nil {
		t.Fatal(err)
	}
	if _, err := Show(root, "r7", namesOf("a", "b")); err == nil || !strings.Contains(err.Error(), "a, b") {
		t.Fatalf("two matches must refuse and name them: %v", err)
	}
}

func TestShow_NoImageStoreIsRefused(t *testing.T) {
	root := newRoot(t)
	if _, err := Install(root, recordJSON(t, "r8"), false); err != nil {
		t.Fatal(err)
	}
	if _, err := Show(root, "r8", nil); err == nil {
		t.Fatal("show ran with no image store")
	}
}

func TestInstall_ReinstallingTheSameRecordWithDefaultRepairsADamagedPointer(t *testing.T) {
	root := newRoot(t)
	raw := recordJSON(t, "d1")
	if _, err := Install(root, raw, true); err != nil {
		t.Fatal(err)
	}
	pin := filepath.Join(root, "review", "maps", "_default.sha256")
	if err := os.WriteFile(pin, []byte("damaged\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := DefaultID(root); err == nil {
		t.Fatal("a damaged pointer pin was accepted")
	}
	if _, err := Install(root, raw, true); err != nil {
		t.Fatalf("the repair the hint names does not work: %v", err)
	}
	if id, err := DefaultID(root); err != nil || id != "d1" {
		t.Fatalf("pointer after repair: %q %v", id, err)
	}
}

func TestInstall_ReinstallingTheSameRecordWithoutDefaultIsAnAcceptedNoOp(t *testing.T) {
	root := newRoot(t)
	raw := recordJSON(t, "d2")
	first, err := Install(root, raw, false)
	if err != nil {
		t.Fatal(err)
	}
	again, err := Install(root, raw, false)
	if err != nil || again != first {
		t.Fatalf("%+v %v", again, err)
	}
}

func TestInstall_ADifferentRecordUnderAnInstalledIDIsStillRefusedWithDefault(t *testing.T) {
	root := newRoot(t)
	if _, err := Install(root, recordJSON(t, "d3"), true); err != nil {
		t.Fatal(err)
	}
	other := goodMap("d3")
	other.Argv = []string{"--other"}
	raw, _ := json.Marshal(other)
	if _, err := Install(root, raw, true); err == nil {
		t.Fatal("a different record replaced an installed one")
	}
}

func TestShow_AsksTheStoreForTheMapsImageDigestUnderTheHostRoot(t *testing.T) {
	root := newRoot(t)
	m := goodMap("s1")
	m.BindingSHA256 = strings.Repeat("a", 64) // differs from the image digest on purpose
	raw, _ := json.Marshal(m)
	if _, err := Install(root, raw, false); err != nil {
		t.Fatal(err)
	}
	calls := 0
	names := func(hr, digest string) ([]string, error) {
		calls++
		if hr != root || digest != m.ImageDigest {
			return nil, nil
		}
		return []string{"reviewer"}, nil
	}
	info, err := Show(root, "s1", names)
	if err != nil || calls != 1 || info.ImageRef != "reviewer@"+m.ImageDigest {
		t.Fatalf("calls=%d %+v %v", calls, info, err)
	}
}
