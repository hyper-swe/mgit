package main

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/hyper-swe/mgit/internal/reviewbind"
)

func runMapCmd(t *testing.T, args ...string) (string, error) {
	t.Helper()
	cmd := sandboxReviewMapCmd()
	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(&out)
	cmd.SetArgs(args)
	err := cmd.Execute()
	return out.String(), err
}

func TestReviewMapCLI_InstallThenShowTheDefault(t *testing.T) {
	old := mapImageNames
	mapImageNames = func(string, string) ([]string, error) { return []string{"reviewer"}, nil }
	defer func() { mapImageNames = old }()
	root := filepath.Join(t.TempDir(), "host")
	rec := filepath.Join(t.TempDir(), "map.json")
	body := `{"schema":"mgit.review-map/v1","id":"std","binding_sha256":"` + strings.Repeat("a", 64) + `","image_digest":"sha256:` + strings.Repeat("b", 64) +
		`","guest_entrypoint":"/opt/reviewer/bin/review","entrypoint_sha256":"` + strings.Repeat("c", 64) + `","argv":["--json"],"egress_policy_id":"none"}`
	if err := os.WriteFile(rec, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := runMapCmd(t, "show", "--host-root", root, "--json"); err == nil || !strings.Contains(err.Error(), "map install --file <map.json> --default") {
		t.Fatalf("an unconfigured host must be told how to install: %v", err)
	}
	if _, err := runMapCmd(t, "install", "--host-root", root, "--file", rec, "--default"); err != nil {
		t.Fatal(err)
	}
	out, err := runMapCmd(t, "show", "--host-root", root, "--json")
	if err != nil {
		t.Fatal(err)
	}
	var info reviewbind.Info
	if err := json.Unmarshal([]byte(out), &info); err != nil || info.ID != "std" || info.BindingSHA256 != strings.Repeat("a", 64) || info.MapSHA256 == info.BindingSHA256 || info.ImageDigest != "sha256:"+strings.Repeat("b", 64) || info.ImageRef != "reviewer@sha256:"+strings.Repeat("b", 64) || info.Schema != reviewbind.InfoSchema {
		t.Fatalf("show --json: %s %v", out, err)
	}
}
