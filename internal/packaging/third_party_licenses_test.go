package packaging

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// The release archives ship the license texts of the Go modules compiled into
// their binaries, in one generated file, so an MIT or BSD dependency's notice
// travels with the binary that contains it. A module with no license text
// stops the build rather than shipping without one. Refs: MGIT-306

func TestThirdPartyLicensesScript_CarriesTheLicenseTextOfEveryCompiledInModule(t *testing.T) {
	skipOnWindows(t)
	out := filepath.Join(t.TempDir(), "THIRD_PARTY_LICENSES.txt")
	log, err := runRepoScript(t, repoRoot(t), "third-party-licenses.sh", nil, out)
	require.NoError(t, err, "%s", log)
	body, readErr := os.ReadFile(out) //nolint:gosec // a t.TempDir path
	require.NoError(t, readErr)
	text := string(body)

	for _, mod := range []string{
		"github.com/labstack/echo/v4",
		"github.com/mark3labs/mcp-go",
		"modernc.org/sqlite",
		"golang.org/x/sys",
		"github.com/spf13/cobra",
	} {
		assert.Contains(t, text, "== "+mod+" ", "the file must name %s", mod)
	}
	assert.Contains(t, text, "Permission is hereby granted", "an MIT dependency's text must be present")
	assert.Contains(t, text, "Redistribution and use in source and binary forms", "a BSD dependency's text must be present")
	assert.NotContains(t, text, "== github.com/hyper-swe/mgit ", "mgit's own license is shipped as LICENSE, not listed here")
}

func TestThirdPartyLicensesScript_AModuleWithNoLicenseTextStopsTheBuild(t *testing.T) {
	skipOnWindows(t)
	dir := t.TempDir()
	bare := filepath.Join(dir, "bare")
	require.NoError(t, os.MkdirAll(bare, 0o750))
	require.NoError(t, os.WriteFile(filepath.Join(bare, "x.go"), []byte("package x\n"), 0o600))
	list := filepath.Join(dir, "modules.tsv")
	require.NoError(t, os.WriteFile(list, []byte("example.com/bare\tv1.0.0\t"+bare+"\n"), 0o600))

	out := filepath.Join(dir, "out.txt")
	log, err := runRepoScript(t, repoRoot(t), "third-party-licenses.sh", []string{"MGIT_LICENSES_MODULES_FILE=" + list}, out)
	require.Error(t, err, "a module with no license file must refuse: %s", log)
	assert.Contains(t, log, "example.com/bare")
	_, statErr := os.Stat(out)
	assert.True(t, os.IsNotExist(statErr), "no partial file may be left behind")
}

func TestGoreleaser_ShipsTheGeneratedThirdPartyLicensesInEveryArchive(t *testing.T) {
	cfg := readRepoFile(t, ".goreleaser.yaml")
	assert.Contains(t, cfg, "scripts/release/third-party-licenses.sh build/THIRD_PARTY_LICENSES.txt",
		"a before hook must generate the file")
	assert.True(t, strings.Contains(cfg, "src: build/THIRD_PARTY_LICENSES.txt") || strings.Contains(cfg, `src: "build/THIRD_PARTY_LICENSES.txt"`),
		"the archive must carry the generated file")
}

// goreleaser refuses to build when dist/ is not empty, and before hooks run
// before the build: a hook that writes into dist/ stops the release. The
// generated file lives in build/, which is ignored, and is read from there.
func TestGoreleaser_NoBeforeHookWritesIntoDist(t *testing.T) {
	cfg := readRepoFile(t, ".goreleaser.yaml")
	hooks, ok := cutSection(cfg, "before:\n  hooks:\n", "\nbuilds:")
	require.True(t, ok, "the before hooks must be found")
	for _, line := range strings.Split(hooks, "\n") {
		trimmed := strings.TrimSpace(line)
		if !strings.HasPrefix(trimmed, "- ") {
			continue
		}
		assert.NotContains(t, trimmed, "dist/", "a before hook must not write into dist/: goreleaser then refuses the build (%q)", trimmed)
	}
}
