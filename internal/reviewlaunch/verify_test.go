package reviewlaunch

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func writeTemp(t *testing.T, content []byte) *os.File {
	t.Helper()
	p := filepath.Join(t.TempDir(), "entry")
	if err := os.WriteFile(p, content, 0o755); err != nil {
		t.Fatal(err)
	}
	f, err := os.Open(p)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { f.Close() })
	return f
}

func sum(b []byte) string { h := sha256.Sum256(b); return hex.EncodeToString(h[:]) }

func TestVerifyEntrypoint_Refusals(t *testing.T) {
	elf := append([]byte("\x7fELF"), []byte("rest of a pretend binary")...)
	cases := []struct {
		name    string
		content []byte
		want    string
		code    string
	}{
		{"matching hash passes", elf, sum(elf), ""},
		{"other bytes: hash mismatch", elf, sum([]byte("other")), CodeHashMismatch},
		{"malformed digest", elf, "xyz", CodeBadDigest},
		{"empty digest", elf, "", CodeBadDigest},
		{"upper-case digest is not canonical", elf, strings.ToUpper(sum(elf)), CodeBadDigest},
		{"script is not an ELF", []byte("#!/bin/sh\necho hi\n"), sum([]byte("#!/bin/sh\necho hi\n")), CodeNotELF},
	}
	for _, c := range cases {
		err := VerifyEntrypoint(writeTemp(t, c.content), c.want)
		if c.code == "" {
			if err != nil {
				t.Errorf("%s: %v", c.name, err)
			}
			continue
		}
		var r *Refusal
		if !errors.As(err, &r) || r.Code != c.code {
			t.Errorf("%s: err %v, want refusal %s", c.name, err, c.code)
		}
	}
}

func TestRefusalJSON_CarriesCodeAndNotStarted(t *testing.T) {
	got := string(RefusalJSON(&Refusal{Code: CodeHashMismatch, Detail: "digest differs"}))
	for _, want := range []string{`"schema":"mgit.review-error/v1"`, `"code":"HASH_MISMATCH"`, `"phase":"launch"`, `"reviewer_started":false`} {
		if !strings.Contains(got, want) {
			t.Errorf("missing %s in %s", want, got)
		}
	}
}
