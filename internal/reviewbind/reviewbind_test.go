package reviewbind

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/hyper-swe/mgit/internal/reviewprep"
)

func goodMap(id string) reviewprep.Mapping {
	return reviewprep.Mapping{
		Schema: reviewprep.MapSchema, ID: id,
		BindingSHA256:    strings.Repeat("a", 64),
		ImageDigest:      "sha256:" + strings.Repeat("b", 64),
		GuestEntrypoint:  "/opt/reviewer/bin/review",
		EntrypointSHA256: strings.Repeat("c", 64),
		Argv:             []string{"--json"},
		EgressPolicyID:   "none",
	}
}

// install writes the record and its pinned canonical hash the way an operator does.
func install(t *testing.T, root string, m reviewprep.Mapping) {
	t.Helper()
	dir := filepath.Join(root, "review", "maps")
	if err := os.MkdirAll(dir, 0o750); err != nil {
		t.Fatal(err)
	}
	raw, err := json.Marshal(m)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, m.ID+".json"), raw, 0o600); err != nil {
		t.Fatal(err)
	}
	h, err := CanonicalHash(m)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, m.ID+".sha256"), []byte(h+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
}

func wantMismatch(t *testing.T, err error) {
	t.Helper()
	var e *reviewprep.Error
	if !errors.As(err, &e) || e.Code != reviewprep.CodeMappingMismatch {
		t.Fatalf("want a MAPPING_MISMATCH refusal, got %v", err)
	}
}

func TestLoadMapReadsAnInstalledMapWhoseHashMatchesItsPin(t *testing.T) {
	root := t.TempDir()
	install(t, root, goodMap("pi"))
	m, err := LoadMap(root, "pi")
	if err != nil || m.ID != "pi" || m.EgressPolicyID != "none" {
		t.Fatalf("got %+v, %v", m, err)
	}
}

func TestLoadMapRefusesARecordEditedAfterItWasPinned(t *testing.T) {
	root := t.TempDir()
	m := goodMap("pi")
	install(t, root, m)
	m.Argv = []string{"--json", "--extra"}
	raw, _ := json.Marshal(m)
	if err := os.WriteFile(filepath.Join(root, "review", "maps", "pi.json"), raw, 0o600); err != nil {
		t.Fatal(err)
	}
	_, err := LoadMap(root, "pi")
	wantMismatch(t, err)
}

func TestLoadMapRefusesAMapWithNoPin(t *testing.T) {
	root := t.TempDir()
	install(t, root, goodMap("pi"))
	if err := os.Remove(filepath.Join(root, "review", "maps", "pi.sha256")); err != nil {
		t.Fatal(err)
	}
	_, err := LoadMap(root, "pi")
	wantMismatch(t, err)
}

func TestLoadMapRefusesARecordThatNamesAnotherId(t *testing.T) {
	root := t.TempDir()
	m := goodMap("other")
	install(t, root, m)
	dir := filepath.Join(root, "review", "maps")
	for _, ext := range []string{".json", ".sha256"} {
		if err := os.Rename(filepath.Join(dir, "other"+ext), filepath.Join(dir, "pi"+ext)); err != nil {
			t.Fatal(err)
		}
	}
	_, err := LoadMap(root, "pi")
	wantMismatch(t, err)
}

func TestLoadMapRefusesASymlinkedOrGroupWritableRecord(t *testing.T) {
	root := t.TempDir()
	install(t, root, goodMap("pi"))
	p := filepath.Join(root, "review", "maps", "pi.json")
	if err := os.Chmod(p, 0o666); err != nil { //nolint:gosec // group- and world-writable on purpose: the record must be refused
		t.Fatal(err)
	}
	_, err := LoadMap(root, "pi")
	wantMismatch(t, err)
	if err := os.Chmod(p, 0o600); err != nil {
		t.Fatal(err)
	}
	real := filepath.Join(root, "elsewhere.json")
	if err := os.Rename(p, real); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(real, p); err != nil {
		t.Fatal(err)
	}
	_, err = LoadMap(root, "pi")
	wantMismatch(t, err)
}

func TestLoadMapRefusesAnIdThatLeavesTheMapDirectory(t *testing.T) {
	root := t.TempDir()
	for _, id := range []string{"", "../x", "a/b", "..", "PI space"} {
		_, err := LoadMap(root, id)
		wantMismatch(t, err)
	}
}

func TestLoadMapRefusesAMapWhoseEgressOrCredentialIsMalformed(t *testing.T) {
	for _, mut := range []func(*reviewprep.Mapping){
		func(m *reviewprep.Mapping) { m.EgressPolicyID = "host:a.example,b.example" },
		func(m *reviewprep.Mapping) { m.EgressPolicyID = "any" },
		func(m *reviewprep.Mapping) { m.CredentialRef = "has space" },
		func(m *reviewprep.Mapping) { m.CredentialRef = "slot1"; m.EgressPolicyID = "none" }, // a credential with no network is meaningless
	} {
		root := t.TempDir()
		m := goodMap("pi")
		mut(&m)
		install(t, root, m)
		_, err := LoadMap(root, "pi")
		wantMismatch(t, err)
	}
}

func TestCanonicalHashIgnoresKeyOrderAndSpacing(t *testing.T) {
	m := goodMap("pi")
	h1, err := CanonicalHash(m)
	if err != nil {
		t.Fatal(err)
	}
	var generic map[string]any
	raw, _ := json.MarshalIndent(m, "  ", "\t")
	if err := json.Unmarshal(raw, &generic); err != nil {
		t.Fatal(err)
	}
	h2, err := CanonicalHashJSON(raw)
	if err != nil || h1 != h2 {
		t.Fatalf("%s vs %s (%v)", h1, h2, err)
	}
	m.Argv = []string{"--other"}
	if h3, _ := CanonicalHash(m); h3 == h1 {
		t.Fatal("a changed argv must change the hash")
	}
}

func TestEgressPolicyNoneAllowsNothingAndAHostAllowsOnlyThatHost(t *testing.T) {
	for _, id := range []string{"", "none"} {
		p, err := ParseEgress(id)
		if err != nil || p.Allows("api.example.com") {
			t.Fatalf("%q: %+v %v", id, p, err)
		}
	}
	p, err := ParseEgress("host:api.example.com")
	if err != nil || !p.Allows("api.example.com") || !p.Allows("API.example.com") {
		t.Fatalf("%+v %v", p, err)
	}
	for _, h := range []string{"evil.example.com", "api.example.com.evil.net", "sub.api.example.com", "", "api.example.com:443@evil"} {
		if p.Allows(h) {
			t.Errorf("allowed %q", h)
		}
	}
}

func TestParseEgressRefusesAnythingButNoneOrOneNamedHost(t *testing.T) {
	for _, id := range []string{"host:", "host:*.example.com", "host:1.2.3.4", "host:a b", "host:a.example,b.example", "all", "host:localhost", "host:a.example/path", "HOST:a.example"} {
		if _, err := ParseEgress(id); err == nil {
			t.Errorf("%q was accepted", id)
		}
	}
}

const fake = "FAKE-KEY-0123456789-not-real"

func TestSlotIsFilledOnceAndTakenOnce(t *testing.T) {
	s := NewSlot("slot1")
	if _, err := s.Take(); err == nil {
		t.Fatal("an empty slot gave a value")
	}
	if err := s.Fill(strings.NewReader(fake)); err != nil {
		t.Fatal(err)
	}
	if err := s.Fill(strings.NewReader("second")); err == nil {
		t.Fatal("a slot was filled twice")
	}
	b, err := s.Take()
	if err != nil || string(b) != fake {
		t.Fatalf("%q %v", b, err)
	}
	if _, err := s.Take(); err == nil {
		t.Fatal("a slot gave its value twice")
	}
}

func TestSlotRefusesAnOversizedOrEmptySecret(t *testing.T) {
	if err := NewSlot("s").Fill(strings.NewReader("")); err == nil {
		t.Error("empty secret accepted")
	}
	if err := NewSlot("s").Fill(bytes.NewReader(bytes.Repeat([]byte("x"), MaxSecret+1))); err == nil {
		t.Error("oversized secret accepted")
	}
}

func TestTheSecretNeverAppearsInAnyRenderingOrError(t *testing.T) {
	s := NewSlot("slot1")
	if err := s.Fill(strings.NewReader(fake)); err != nil {
		t.Fatal(err)
	}
	var sb strings.Builder
	for _, f := range []string{"%v", "%+v", "%#v", "%s", "%q", "%x"} {
		fmt.Fprintf(&sb, f, s)
		fmt.Fprintf(&sb, f, &s)
	}
	j, _ := json.Marshal(s)
	sb.Write(j)
	sb.WriteString(s.String())
	if strings.Contains(sb.String(), fake) || strings.Contains(sb.String(), fmt.Sprintf("%x", fake)) {
		t.Fatalf("the secret leaked: %s", sb.String())
	}
	err := NewSlot("s2").Fill(errReader{fake})
	if err == nil || strings.Contains(err.Error(), fake) {
		t.Fatalf("a read error carried the secret: %v", err)
	}
}

type errReader struct{ s string }

func (e errReader) Read([]byte) (int, error) { return 0, errors.New("read failed near " + e.s[:0]) }

func TestSlotFillsFromAnInheritedFdAndNotFromArgvOrEnv(t *testing.T) {
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := w.WriteString(fake); err != nil {
		t.Fatal(err)
	}
	_ = w.Close()
	s := NewSlot("slot1")
	if err := s.FillFromFile(r); err != nil {
		t.Fatal(err)
	}
	for _, a := range os.Args {
		if strings.Contains(a, fake) {
			t.Fatal("secret in argv")
		}
	}
	for _, e := range os.Environ() {
		if strings.Contains(e, fake) {
			t.Fatal("secret in env")
		}
	}
	if b, _ := s.Take(); string(b) != fake {
		t.Fatal("not read")
	}
}

func TestLoadMapAcceptsEveryIdTheServiceLetsThrough(t *testing.T) {
	root := t.TempDir()
	install(t, root, goodMap("Pi-Review.v1"))
	if m, err := LoadMap(root, "Pi-Review.v1"); err != nil || m.ID != "Pi-Review.v1" {
		t.Fatalf("%+v %v", m, err)
	}
}
