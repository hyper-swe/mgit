package e2e

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// The real base-resolve CLI uses a loopback proxy as its entire registry.
// No guest, layers, image download, authentication, or external network is
// needed to verify the same reference/manifest path base-from uses.
func TestE2E_RegistryAlias_Offline(t *testing.T) {
	bin := buildMgitBinary(t)
	root := t.TempDir()
	body := `{"schemaVersion":2,"mediaType":"application/vnd.oci.image.manifest.v1+json","config":{"digest":"sha256:` + strings.Repeat("b", 64) + `"},"layers":[{"digest":"sha256:` + strings.Repeat("c", 64) + `","size":1}]}`
	hash := sha256.Sum256([]byte(body))
	digest := "sha256:" + hex.EncodeToString(hash[:])
	var mu sync.Mutex
	status := http.StatusOK
	var requests []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		requests = append(requests, r.URL.Host+r.URL.Path)
		// This proxy never forwards requests, even on a regression.
		w.WriteHeader(status)
		_, _ = fmt.Fprint(w, body)
	}))
	defer srv.Close()
	for _, key := range []string{"HTTP_PROXY", "http_proxy", "HTTPS_PROXY", "https_proxy"} {
		t.Setenv(key, srv.URL)
	}
	for _, key := range []string{"NO_PROXY", "no_proxy"} {
		t.Setenv(key, "")
	}
	for _, name := range []string{"docker.io/debian", "docker.io/library/debian", "registry-1.docker.io/library/debian"} {
		out, err := runMgit(t, bin, root, "sandbox", "base", "resolve", name+"@"+digest, "--plain-http")
		require.NoError(t, err, out)
		assert.Equal(t, "registry-1.docker.io/library/debian@"+digest, strings.TrimSpace(out))
	}
	mu.Lock()
	got := append([]string(nil), requests...)
	mu.Unlock()
	require.Len(t, got, 3)
	for _, request := range got {
		assert.Equal(t, "registry-1.docker.io/v2/library/debian/manifests/"+digest, request)
	}
	for _, bad := range []struct {
		body   string
		status int
	}{
		{"<html>registry landing page</html>", http.StatusOK},
		{`{"errors":[{"code":"DENIED"}]}`, http.StatusOK},
		{"<html>not found</html>", http.StatusNotFound},
	} {
		mu.Lock()
		body, status = bad.body, bad.status
		mu.Unlock()
		out, err := runMgit(t, bin, root, "sandbox", "base", "resolve", "docker.io/debian@"+digest, "--plain-http")
		require.Error(t, err)
		assert.Contains(t, out, "registry-1.docker.io")
		assert.Contains(t, out, fmt.Sprint(bad.status))
		assert.NotContains(t, out, "hashes to")
	}
}
