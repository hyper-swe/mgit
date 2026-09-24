package main

import (
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Measured on the released v0.6.8: `mgit sandbox image install` with no
// --from died on "read manifest: fetch https://github.com/hyper-swe/mgit/
// releases/latest/download/manifest.json: HTTP 404". Publishing image
// bundles with releases is on hold (docs/INSTALL-SANDBOX.md), so that 404 is
// the expected state, not an outage, and the error named neither that nor
// the way forward. The default source answering 404 now says releases carry
// no bundle and names --from, and the libkrun way to a base. An explicit
// --from that 404s is that source's problem, and no claim about releases is
// made. Refs: MGIT-234
func TestImageInstall_TheDefaultSourceWithoutABundle_SaysSoAndNamesFrom(t *testing.T) {
	srv := httptest.NewServer(http.NotFoundHandler())
	defer srv.Close()
	repo := newRepo(t)
	_, err := runImage(t, repo, "init")
	require.NoError(t, err)
	t.Chdir(repo)

	var out bytes.Buffer
	err = installImage(context.Background(), &out, imageInstallArgs{source: srv.URL, defaulted: true, name: "base"})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "HTTP 404", "the evidence stays")
	assert.Contains(t, err.Error(), "releases do not carry a guest image bundle")
	assert.Contains(t, err.Error(), "--from <dir-or-url>")
	assert.Contains(t, err.Error(), "mgit sandbox base from", "the libkrun way to a base")

	err = installImage(context.Background(), &out, imageInstallArgs{source: srv.URL, defaulted: false, name: "base"})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "HTTP 404")
	assert.NotContains(t, err.Error(), "releases do not carry", "an explicit --from is not the release")
}
