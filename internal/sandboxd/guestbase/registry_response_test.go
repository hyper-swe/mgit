package guestbase

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestParseRef_DockerHubAlias(t *testing.T) {
	digest := "sha256:" + strings.Repeat("a", 64)
	for _, name := range []string{"debian", "library/debian", "acme/base"} {
		for _, suffix := range []string{"", ":12", "@" + digest} {
			t.Run(name+suffix, func(t *testing.T) {
				got, err := ParseRef("docker.io/" + name + suffix)
				if err != nil {
					t.Fatal(err)
				}
				want, err := ParseRef("registry-1.docker.io/" + name + suffix)
				if err != nil {
					t.Fatal(err)
				}
				if got != want {
					t.Fatalf("alias = %+v; registry host = %+v", got, want)
				}
			})
		}
	}
	// An unrelated registry's single-name repository must not gain library/.
	got, err := ParseRef("example.com/debian@" + digest)
	if err != nil {
		t.Fatal(err)
	}
	if got.Registry != "example.com" || got.Repository != "debian" {
		t.Fatalf("unrelated registry changed: %+v", got)
	}
}

func TestFetchManifest_NonManifestResponse(t *testing.T) {
	for _, tt := range []struct {
		name, body string
		status     int
	}{
		{"HTML success", "<html>registry landing page</html>", http.StatusOK},
		{"JSON error success", `{"errors":[{"code":"DENIED"}]}`, http.StatusOK},
		{"empty success", "", http.StatusOK},
		{"HTML not found", "<html>not found</html>", http.StatusNotFound},
		{"unsupported schema", `{"schemaVersion":1,"layers":[]}`, http.StatusOK},
	} {
		t.Run(tt.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(tt.status); _, _ = fmt.Fprint(w, tt.body) }))
			defer srv.Close()
			host := strings.TrimPrefix(srv.URL, "http://")
			c := client{http: srv.Client(), scheme: "http"}
			ref := Ref{Registry: host, Repository: "library/debian"}
			_, _, err := c.fetchManifest(context.Background(), ref, "sha256:"+strings.Repeat("a", 64))
			if err == nil {
				t.Fatal("non-manifest accepted")
			}
			for _, want := range []string{host, fmt.Sprint(tt.status)} {
				if !strings.Contains(err.Error(), want) {
					t.Errorf("error %q lacks %q", err, want)
				}
			}
			if strings.Contains(err.Error(), "hashes to") {
				t.Fatalf("non-manifest reported as digest mismatch: %v", err)
			}
		})
	}
}

func TestFetchManifest_ValidManifestStillChecksDigest(t *testing.T) {
	body := `{"schemaVersion":2,"mediaType":"application/vnd.oci.image.manifest.v1+json","config":{"digest":"sha256:` + strings.Repeat("b", 64) + `"},"layers":[]}`
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { _, _ = fmt.Fprint(w, body) }))
	defer srv.Close()
	c := client{http: srv.Client(), scheme: "http"}
	ref := Ref{Registry: strings.TrimPrefix(srv.URL, "http://"), Repository: "library/debian"}
	_, _, err := c.fetchManifest(context.Background(), ref, "sha256:"+strings.Repeat("a", 64))
	if err == nil || !strings.Contains(err.Error(), "hashes to") {
		t.Fatalf("valid manifest must retain digest check: %v", err)
	}
}
