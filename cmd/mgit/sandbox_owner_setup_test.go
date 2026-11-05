package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestOwnerSetupRealCLIRefusal(t *testing.T) {
	bin := buildMgitTestBinary(t)
	for _, verb := range []string{"base from", "image add"} {
		for _, marker := range []string{"linked", "sandbox"} {
			for _, trust := range []bool{false, true} {
				name := verb + "/" + marker + "/absent-trust"
				if trust {
					name = verb + "/" + marker + "/existing-pin"
				}
				t.Run(name, func(t *testing.T) {
					repo, wt := baseOwnerFixture(t, marker)
					if trust {
						_, err := initTrustRoot(t, repo)
						require.NoError(t, err)
						_, err = runBase(t, repo, "set", userspaceTree(t), "--guest-bin-dir", fakeGuestBins(t))
						require.NoError(t, err)
					}
					nested := filepath.Join(wt, "nested")
					require.NoError(t, os.Mkdir(nested, 0o700))
					kernel, rootfs := writeImageFiles(t)
					bins := fakeGuestBins(t)
					cache := os.Getenv("MGIT_BASE_CACHE")
					var requests atomic.Int64
					srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
						requests.Add(1)
						http.Error(w, "unexpected registry request", http.StatusForbidden)
					}))
					defer srv.Close()
					roots := []string{repo, wt, cache, filepath.Dir(kernel), bins}
					before := make([]map[string]string, len(roots))
					for i, root := range roots {
						before[i] = baseOwnerSnapshot(t, root)
					}
					for _, cwd := range []string{wt, nested} {
						args := []string{"sandbox", "image", "add", "--name", "base", "--kernel", kernel, "--rootfs", rootfs, "--json"}
						if verb == "base from" {
							args = []string{"sandbox", "base", "from", strings.TrimPrefix(srv.URL, "http://") + "/acme/base:test", "--plain-http", "--guest-bin-dir", bins, "--json"}
						}
						ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
						cmd := exec.CommandContext(ctx, bin, args...) //nolint:gosec // test-built binary and fixture arguments
						cmd.Dir = cwd
						output, err := cmd.CombinedOutput()
						cancel()
						assert.Error(t, err)
						assert.Contains(t, string(output), canonicalPath(repo))
						assert.Contains(t, string(output), canonicalPath(wt))
						assert.Contains(t, string(output), "run `mgit sandbox "+verb+"`")
						assert.NotContains(t, string(output), "image_ref")
						assert.Zero(t, requests.Load(), "refusal must precede registry traffic")
						for i, root := range roots {
							assert.Equal(t, before[i], baseOwnerSnapshot(t, root), "fixture must not change: %s", root)
						}
					}
				})
			}
		}
	}
}

func TestOwnerSetupExplicitOwner(t *testing.T) {
	for _, shape := range []string{"root", "subdirectory", "symlink"} {
		t.Run(shape, func(t *testing.T) {
			repo := newRepo(t)
			_, err := initTrustRoot(t, repo)
			require.NoError(t, err)
			cwd := repo
			if shape == "subdirectory" {
				cwd = filepath.Join(repo, "sub")
				require.NoError(t, os.Mkdir(cwd, 0o700))
			}
			if shape == "symlink" {
				cwd = filepath.Join(t.TempDir(), "alias")
				require.NoError(t, os.Symlink(repo, cwd))
			}
			kernel, rootfs := writeImageFiles(t)
			out, err := runImage(t, cwd, "add", "--name", "control", "--kernel", kernel, "--rootfs", rootfs, "--json")
			require.NoError(t, err)
			assert.Contains(t, out, "image_ref")
			srv, ref := fakeImageServer(t, map[string]string{"bin/sh": "#!/bin/sh", "etc/os-release": "ID=alpine"})
			defer srv.Close()
			out, err = runBase(t, cwd, "from", ref, "--plain-http", "--guest-bin-dir", fakeGuestBins(t), "--json")
			require.NoError(t, err)
			assert.Contains(t, out, "image_ref")
		})
	}
}
